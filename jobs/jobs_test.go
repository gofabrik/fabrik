package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

type Email struct {
	To string `json:"to"`
}

type OrderPlaced struct {
	ID int `json:"id"`
}

func testManager(t *testing.T, store Store) *Manager {
	t.Helper()
	m, err := New(store, Config{
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		DefaultBackoff: ExponentialBackoff{Base: time.Millisecond, Max: 5 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

func runWorker(t *testing.T, m *Manager, cfg WorkerConfig) *Worker {
	t.Helper()
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 5 * time.Millisecond
	}
	if cfg.LeaseDuration == 0 {
		cfg.LeaseDuration = 150 * time.Millisecond
	}
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = 30 * time.Millisecond
	}
	if cfg.SweepInterval == 0 {
		cfg.SweepInterval = 20 * time.Millisecond
	}
	w, err := NewWorker(m, cfg)
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	go func() { _ = w.Start(context.Background()) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		w.Stop(ctx)
	})
	return w
}

func eventually(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met: %s", msg)
}

func jobState(t *testing.T, m *Manager, id string) State {
	t.Helper()
	info, err := m.GetJob(context.Background(), id)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	return info.State
}

func TestRegistrationConflicts(t *testing.T) {
	m := testManager(t, NewMemoryStore())
	if err := Register[Email](m, "email"); err != nil {
		t.Fatal(err)
	}
	if err := Register[Email](m, "email"); err != nil {
		t.Fatalf("idempotent re-register should be nil: %v", err)
	}
	if err := Register[Email](m, "email2"); !errors.Is(err, ErrTypeAlreadyRegistered) {
		t.Fatalf("want ErrTypeAlreadyRegistered, got %v", err)
	}
	if err := Register[OrderPlaced](m, "email"); !errors.Is(err, ErrKindAlreadyRegistered) {
		t.Fatalf("want ErrKindAlreadyRegistered, got %v", err)
	}
	noop := func(Context, Email) error { return nil }
	if err := On[Email](m, "h", noop); err != nil {
		t.Fatal(err)
	}
	if err := On[Email](m, "h", noop); !errors.Is(err, ErrHandlerAlreadyRegistered) {
		t.Fatalf("want ErrHandlerAlreadyRegistered, got %v", err)
	}
}

func TestStopBeforeStartDoesNotHang(t *testing.T) {
	m := testManager(t, NewMemoryStore())
	Handle[Email](m, "email", func(Context, Email) error { return nil })
	w, err := NewWorker(m, WorkerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { w.Stop(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop before Start hung")
	}
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("Start after Stop: %v", err)
	}
}

func TestRegisterRejectsReservedCronPrefix(t *testing.T) {
	m := testManager(t, NewMemoryStore())
	if err := Register[Email](m, "cron:foo"); err == nil {
		t.Fatal("Register must reject the reserved cron: prefix")
	}
}

func TestRegisterRejectsPointerMessage(t *testing.T) {
	m := testManager(t, NewMemoryStore())
	if err := Register[*Email](m, "email"); err == nil {
		t.Fatal("Register must reject a pointer message type")
	}
}

func TestRegisterRejectsNonStructMessage(t *testing.T) {
	type Tags []string
	type UserID int64
	m := testManager(t, NewMemoryStore())
	if err := Register[Tags](m, "tags"); err == nil {
		t.Fatal("Register must reject a non-struct (slice) message type")
	}
	if err := Register[UserID](m, "user"); err == nil {
		t.Fatal("Register must reject a non-struct (named scalar) message type")
	}
}

func TestEnqueuePublishScheduleRejectPointer(t *testing.T) {
	m := testManager(t, NewMemoryStore())
	if err := Handle[Email](m, "email", func(Context, Email) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Enqueue(context.Background(), &Email{}); err == nil {
		t.Fatal("Enqueue must reject a pointer message")
	}
	if _, err := m.Publish(context.Background(), &Email{}); err == nil {
		t.Fatal("Publish must reject a pointer message")
	}
	if err := m.Schedule("poll", Every(time.Minute), &Email{}, ScheduleOptions{}); err == nil {
		t.Fatal("Schedule must reject a pointer message")
	}
	if _, err := m.Enqueue(context.Background(), (*Email)(nil)); err == nil {
		t.Fatal("Enqueue must reject a nil pointer message")
	}
}

func TestStartRejectsSecondStart(t *testing.T) {
	m := testManager(t, NewMemoryStore())
	w, err := NewWorker(m, WorkerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstErr := make(chan error, 1)
	go func() { firstErr <- w.Start(ctx) }()
	eventually(t, func() bool { return w.started.Load() }, "worker started")
	if err := w.Start(ctx); !errors.Is(err, ErrWorkerAlreadyStarted) {
		t.Fatalf("second Start = %v, want ErrWorkerAlreadyStarted", err)
	}
	cancel()
	if err := <-firstErr; err != nil {
		t.Fatalf("first Start returned %v", err)
	}
}

func TestEnqueueRunsHandler(t *testing.T) {
	m := testManager(t, NewMemoryStore())
	var got atomic.Value
	if err := Handle[Email](m, "email", func(ctx Context, e Email) error {
		got.Store(e.To)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	runWorker(t, m, WorkerConfig{})
	id, err := m.Enqueue(context.Background(), Email{To: "a@b.c"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return got.Load() == "a@b.c" }, "handler ran")
	eventually(t, func() bool { return jobState(t, m, id) == StateSucceeded }, "job succeeded")
}

func TestEnqueueRequiresExactlyOneHandler(t *testing.T) {
	m := testManager(t, NewMemoryStore())
	Register[OrderPlaced](m, "order.placed")
	if _, err := m.Enqueue(context.Background(), OrderPlaced{ID: 1}); err == nil {
		t.Fatal("want error for zero-handler command")
	}
	On[OrderPlaced](m, "a", func(Context, OrderPlaced) error { return nil })
	On[OrderPlaced](m, "b", func(Context, OrderPlaced) error { return nil })
	if _, err := m.Enqueue(context.Background(), OrderPlaced{ID: 1}); err == nil {
		t.Fatal("want error for multi-handler command")
	}
	if _, err := m.Enqueue(context.Background(), Email{}); !errors.Is(err, ErrUnregistered) {
		t.Fatalf("want ErrUnregistered, got %v", err)
	}
}

func TestPublishFanOut(t *testing.T) {
	m := testManager(t, NewMemoryStore())
	Register[OrderPlaced](m, "order.placed")
	var a, b atomic.Int32
	On[OrderPlaced](m, "email", func(Context, OrderPlaced) error { a.Add(1); return nil })
	On[OrderPlaced](m, "inventory", func(Context, OrderPlaced) error { b.Add(1); return nil })
	runWorker(t, m, WorkerConfig{})
	res, err := m.Publish(context.Background(), OrderPlaced{ID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("want 2 publish results, got %d", len(res))
	}
	eventually(t, func() bool { return a.Load() == 1 && b.Load() == 1 }, "both handlers ran once")
}

func TestUniqueKeyDedup(t *testing.T) {
	m := testManager(t, NewMemoryStore())
	Handle[Email](m, "email", func(Context, Email) error { return nil })
	id1, err := m.Enqueue(context.Background(), Email{To: "x"}, UniqueKey("k1"))
	if err != nil {
		t.Fatal(err)
	}
	id2, err := m.Enqueue(context.Background(), Email{To: "x"}, UniqueKey("k1"))
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("want ErrDuplicate, got %v", err)
	}
	if id2 != id1 {
		t.Fatalf("dup should return existing id %s, got %s", id1, id2)
	}
}

func TestRetryThenDiscardAndLedger(t *testing.T) {
	m := testManager(t, NewMemoryStore())
	var runs atomic.Int32
	Handle[Email](m, "email", func(Context, Email) error {
		runs.Add(1)
		return errors.New("boom")
	})
	runWorker(t, m, WorkerConfig{})
	id, err := m.Enqueue(context.Background(), Email{}, MaxAttempts(2))
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return jobState(t, m, id) == StateDiscarded }, "discarded after retries")
	if got := runs.Load(); got != 2 {
		t.Fatalf("want 2 runs, got %d", got)
	}
	atts, err := m.ListJobAttempts(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(atts) != 2 || atts[0].Attempt != 1 || atts[1].Attempt != 2 {
		t.Fatalf("want 2 numbered attempts, got %+v", atts)
	}
}

func TestPermanentError(t *testing.T) {
	m := testManager(t, NewMemoryStore())
	Handle[Email](m, "email", func(Context, Email) error {
		return errors.Join(ErrPermanent, errors.New("nope"))
	})
	runWorker(t, m, WorkerConfig{})
	id, _ := m.Enqueue(context.Background(), Email{}, MaxAttempts(10))
	eventually(t, func() bool { return jobState(t, m, id) == StateFailed }, "permanent -> failed")
}

func TestCancelRunning(t *testing.T) {
	m := testManager(t, NewMemoryStore())
	started := make(chan struct{})
	Handle[Email](m, "email", func(ctx Context, e Email) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	runWorker(t, m, WorkerConfig{})
	id, _ := m.Enqueue(context.Background(), Email{})
	<-started
	immediate, err := m.CancelJob(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if immediate {
		t.Fatal("running cancel should be deferred, not immediate")
	}
	eventually(t, func() bool { return jobState(t, m, id) == StateCancelled }, "cancelled via heartbeat")
}

func TestTimeoutFail(t *testing.T) {
	m := testManager(t, NewMemoryStore())
	Handle[Email](m, "email", func(ctx Context, e Email) error {
		<-ctx.Done()
		return ctx.Err()
	})
	runWorker(t, m, WorkerConfig{})
	id, _ := m.Enqueue(context.Background(), Email{}, Timeout(20*time.Millisecond), TimeoutAction(TimeoutFail))
	eventually(t, func() bool { return jobState(t, m, id) == StateFailed }, "timeout -> failed")
}

func TestPanicRecovered(t *testing.T) {
	m := testManager(t, NewMemoryStore())
	Handle[Email](m, "email", func(Context, Email) error { panic("boom") })
	runWorker(t, m, WorkerConfig{})
	id, _ := m.Enqueue(context.Background(), Email{}, MaxAttempts(1))
	eventually(t, func() bool { return jobState(t, m, id) == StateDiscarded }, "panic recovered -> discarded")
}

func TestDecodePayloadPark(t *testing.T) {
	store := NewMemoryStore()
	m := testManager(t, store)
	Handle[Email](m, "email", func(Context, Email) error { return nil })
	_, err := store.Insert(context.Background(), []Job{{
		Kind: "email", HandlerID: "email", Payload: []byte("not json"),
		Queue: "default", MaxAttempts: 3, AvailableAt: time.Now(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	runWorker(t, m, WorkerConfig{})
	eventually(t, func() bool {
		page, _ := m.ListJobs(context.Background(), ListFilter{States: []State{StateFailed}})
		return len(page.Jobs) == 1
	}, "decode park -> failed")
	page, _ := m.ListJobs(context.Background(), ListFilter{States: []State{StateFailed}})
	if page.Jobs[0].Error == "" {
		t.Fatal("expected decode error recorded")
	}
}

func TestClaimFilterSkipsUnrunnable(t *testing.T) {
	store := NewMemoryStore()
	m := testManager(t, store)
	Handle[Email](m, "email", func(Context, Email) error { return nil })
	res, _ := store.Insert(context.Background(), []Job{{
		Kind: "email", HandlerID: "other", Payload: []byte(`{}`),
		Queue: "default", MaxAttempts: 3, AvailableAt: time.Now(),
	}})
	id := res[0].ID
	runWorker(t, m, WorkerConfig{})
	time.Sleep(150 * time.Millisecond)
	if st := jobState(t, m, id); st != StateAvailable {
		t.Fatalf("unrunnable job should stay available, got %s", st)
	}
}

func TestOnAttemptFinishCommitted(t *testing.T) {
	store := NewMemoryStore()
	var finishState atomic.Value
	var committed atomic.Bool
	var fired atomic.Bool
	m, err := New(store, Config{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Hooks: Hooks{
			OnAttemptFinish: func(_ context.Context, e AttemptFinishEvent) {
				finishState.Store(e.State)
				committed.Store(e.Committed)
				fired.Store(true)
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	Handle[Email](m, "email", func(Context, Email) error { return nil })
	runWorker(t, m, WorkerConfig{})
	m.Enqueue(context.Background(), Email{})
	eventually(t, func() bool { return fired.Load() }, "OnAttemptFinish fired")
	if finishState.Load() != StateSucceeded || !committed.Load() {
		t.Fatalf("want succeeded+committed, got %v committed=%v", finishState.Load(), committed.Load())
	}
}
