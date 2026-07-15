package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
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

func TestClockControlsTimestamps(t *testing.T) {
	fixed := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	store := NewMemoryStore()
	m, err := New(store, Config{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return fixed },
	})
	if err != nil {
		t.Fatal(err)
	}
	Handle[Email](m, "email", func(Context, Email) error { return nil })
	id, err := m.Enqueue(context.Background(), Email{})
	if err != nil {
		t.Fatal(err)
	}
	info, err := m.GetJob(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	// Persisted timestamps use Config.Now.
	if !info.CreatedAt.Equal(fixed) || !info.UpdatedAt.Equal(fixed) {
		t.Fatalf("timestamps = created %v / updated %v, want injected %v", info.CreatedAt, info.UpdatedAt, fixed)
	}
}

// flakyStore injects transient completion failures.
type flakyStore struct {
	Store
	failCompletes atomic.Int32
}

func (f *flakyStore) Complete(ctx context.Context, jobID, workerID string, now time.Time, o Outcome) (State, error) {
	if f.failCompletes.Add(-1) >= 0 {
		return "", errors.New("transient store failure")
	}
	return f.Store.Complete(ctx, jobID, workerID, now, o)
}

func TestCompleteRetriesTransientFailure(t *testing.T) {
	fs := &flakyStore{Store: NewMemoryStore()}
	fs.failCompletes.Store(2)
	m := testManager(t, fs)
	Handle[Email](m, "email", func(Context, Email) error { return nil })
	// Completion retries continue beyond the original claim deadline.
	runWorker(t, m, WorkerConfig{
		PollInterval: 5 * time.Millisecond, LeaseDuration: 60 * time.Millisecond,
		HeartbeatInterval: 20 * time.Millisecond, SweepInterval: 10 * time.Second,
	})
	id, err := m.Enqueue(context.Background(), Email{})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		i, _ := m.GetJob(context.Background(), id)
		return i != nil && i.State == StateSucceeded
	}, "succeeded after transient completion failures past the original lease")
}

func TestCompleteDoesNotRegressUpdatedAt(t *testing.T) {
	store := NewMemoryStore()
	m := testManager(t, store)
	ctx := context.Background()
	early := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	late := early.Add(time.Hour)
	res, err := store.Insert(ctx, early, []Job{{
		Kind: "email", HandlerID: "email", Payload: []byte("{}"),
		Queue: "default", MaxAttempts: 5, AvailableAt: early,
	}})
	if err != nil || len(res) != 1 {
		t.Fatalf("insert: %v", err)
	}
	id := res[0].ID
	if _, err := store.Claim(ctx, ClaimRequest{
		WorkerID: "w1", Queues: []string{"default"}, Now: early, Lease: time.Minute, Limit: 1,
		Handlers: map[HandlerKey]struct{}{{Kind: "email", HandlerID: "email"}: {}},
	}); err != nil {
		t.Fatal(err)
	}
	// Completion must not replace a newer cancellation timestamp.
	if _, err := store.Cancel(ctx, id, late); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Complete(ctx, id, "w1", early, Outcome{
		State: StateSucceeded, Attempt: 1, AttemptState: AttemptSucceeded, StartedAt: early, FinishedAt: early,
	}); err != nil {
		t.Fatal(err)
	}
	info, err := m.GetJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !info.UpdatedAt.Equal(late) {
		t.Fatalf("updated_at = %v, want %v (must not regress under a concurrent cancel)", info.UpdatedAt, late)
	}
}

func TestCancelWinsOverCompletion(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	now := time.Now().UTC()
	res, err := store.Insert(ctx, now, []Job{{
		Kind: "email", HandlerID: "email", Payload: []byte("{}"),
		Queue: "default", MaxAttempts: 5, AvailableAt: now,
	}})
	if err != nil || len(res) != 1 {
		t.Fatalf("insert: %v", err)
	}
	id := res[0].ID
	if _, err := store.Claim(ctx, ClaimRequest{
		WorkerID: "w1", Queues: []string{"default"}, Now: now, Lease: time.Minute, Limit: 1,
		Handlers: map[HandlerKey]struct{}{{Kind: "email", HandlerID: "email"}: {}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Cancel(ctx, id, now); err != nil {
		t.Fatal(err)
	}
	// A stored cancellation overrides the runner's outcome.
	applied, err := store.Complete(ctx, id, "w1", now, Outcome{
		State: StateSucceeded, Attempt: 1, AttemptState: AttemptSucceeded, StartedAt: now, FinishedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if applied != StateCancelled {
		t.Fatalf("applied = %q, want cancelled (cancel wins over the runner's success)", applied)
	}
}

func TestConcurrentUniqueKeyInsert(t *testing.T) {
	store := NewMemoryStore()
	m := testManager(t, store)
	Handle[Email](m, "email", func(Context, Email) error { return nil })
	const n = 20
	var wg sync.WaitGroup
	ids := make([]string, n)
	dups := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id, err := m.Enqueue(context.Background(), Email{}, UniqueKey("k"))
			ids[i] = id
			dups[i] = errors.Is(err, ErrDuplicate)
		}(i)
	}
	wg.Wait()
	// All callers observe the same live job.
	live := 0
	for i := 0; i < n; i++ {
		if !dups[i] {
			live++
		}
	}
	if live != 1 {
		t.Fatalf("concurrent UniqueKey inserts created %d live jobs, want 1", live)
	}
	for i := 0; i < n; i++ {
		if ids[i] != ids[0] {
			t.Fatalf("id[%d]=%s != id[0]=%s; all should be the one live job", i, ids[i], ids[0])
		}
	}
}

func TestSweepExpiredCancellationWins(t *testing.T) {
	store := NewMemoryStore()
	m := testManager(t, store)
	ctx := context.Background()
	now := time.Now().UTC()
	res, err := store.Insert(ctx, now, []Job{{
		Kind: "email", HandlerID: "email", Payload: []byte("{}"),
		Queue: "default", MaxAttempts: 5, AvailableAt: now,
	}})
	if err != nil || len(res) != 1 {
		t.Fatalf("insert: %v", err)
	}
	id := res[0].ID
	claimed, err := store.Claim(ctx, ClaimRequest{
		WorkerID: "w1", Queues: []string{"default"}, Now: now, Lease: time.Minute, Limit: 1,
		Handlers: map[HandlerKey]struct{}{{Kind: "email", HandlerID: "email"}: {}},
	})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v (n=%d)", err, len(claimed))
	}
	if immediate, err := store.Cancel(ctx, id, now); err != nil || immediate {
		t.Fatalf("cancel of a running job: immediate=%v err=%v", immediate, err)
	}
	// Cancellation remains terminal after lease recovery.
	if _, err := store.SweepExpired(ctx, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	info, err := m.GetJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if info.State != StateCancelled {
		t.Fatalf("state = %q, want %q", info.State, StateCancelled)
	}
}

func TestShutdownTimeoutDefault(t *testing.T) {
	m := testManager(t, NewMemoryStore())
	w, err := NewWorker(m, WorkerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if w.cfg.ShutdownTimeout != 30*time.Second {
		t.Fatalf("zero ShutdownTimeout = %v, want 30s default", w.cfg.ShutdownTimeout)
	}
}

func TestShutdownDeadlineReturns(t *testing.T) {
	m := testManager(t, NewMemoryStore())
	started := make(chan struct{})
	release := make(chan struct{})
	Handle[Email](m, "email", func(c Context, e Email) error {
		close(started)
		<-release
		return nil
	})
	w, err := NewWorker(m, WorkerConfig{
		PollInterval: 5 * time.Millisecond, LeaseDuration: time.Second,
		HeartbeatInterval: 100 * time.Millisecond, SweepInterval: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	startDone := make(chan error, 1)
	go func() { startDone <- w.Start(context.Background()) }()
	if _, err := m.Enqueue(context.Background(), Email{}); err != nil {
		t.Fatal(err)
	}
	<-started
	// Shutdown deadlines bound non-cooperative handlers.
	stopCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := w.Stop(stopCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop = %v, want DeadlineExceeded (handler ignores cancel)", err)
	}
	select {
	case err := <-startDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Start = %v, want DeadlineExceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after the Stop deadline")
	}
	close(release)
}

// decodePause blocks decoding before the handler-start transition.
type decodePause struct {
	entered chan struct{}
	release chan struct{}
}

var decodeHook atomic.Pointer[decodePause]

type blockDecodeMsg struct{}

func (*blockDecodeMsg) UnmarshalJSON([]byte) error {
	if h := decodeHook.Load(); h != nil {
		close(h.entered)
		<-h.release
	}
	return nil
}

func TestHandlerNotStartedAfterAbandon(t *testing.T) {
	pause := &decodePause{entered: make(chan struct{}), release: make(chan struct{})}
	decodeHook.Store(pause)
	defer decodeHook.Store(nil)

	m := testManager(t, NewMemoryStore())
	var ran atomic.Bool
	Handle[blockDecodeMsg](m, "block", func(Context, blockDecodeMsg) error {
		ran.Store(true)
		return nil
	})
	w, err := NewWorker(m, WorkerConfig{
		PollInterval: 5 * time.Millisecond, LeaseDuration: time.Second,
		HeartbeatInterval: 100 * time.Millisecond, SweepInterval: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	startDone := make(chan error, 1)
	go func() { startDone <- w.Start(context.Background()) }()
	if _, err := m.Enqueue(context.Background(), blockDecodeMsg{}); err != nil {
		t.Fatal(err)
	}

	// Drain wins while the attempt is still registered.
	<-pause.entered
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := w.Stop(stopCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop = %v, want DeadlineExceeded", err)
	}
	// The registered to running transition must fail after abandonment.
	close(pause.release)
	<-startDone
	time.Sleep(50 * time.Millisecond)
	if ran.Load() {
		t.Fatal("handler was invoked after its attempt was abandoned")
	}
}

func TestAbandonedRunEmitsFinishHook(t *testing.T) {
	var finishes atomic.Int32
	var committed atomic.Bool
	committed.Store(true)
	store := NewMemoryStore()
	m, err := New(store, Config{
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		DefaultBackoff: ExponentialBackoff{Base: time.Millisecond, Max: 5 * time.Millisecond},
		Hooks: Hooks{OnAttemptFinish: func(_ context.Context, e AttemptFinishEvent) {
			finishes.Add(1)
			committed.Store(e.Committed)
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	Handle[Email](m, "email", func(c Context, e Email) error {
		close(started)
		<-release
		return nil
	})
	w, err := NewWorker(m, WorkerConfig{
		PollInterval: 5 * time.Millisecond, LeaseDuration: time.Second,
		HeartbeatInterval: 100 * time.Millisecond, SweepInterval: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = w.Start(context.Background()) }()
	if _, err := m.Enqueue(context.Background(), Email{}); err != nil {
		t.Fatal(err)
	}
	<-started
	stopCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := w.Stop(stopCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop = %v, want DeadlineExceeded", err)
	}
	// An abandoned run reports an uncommitted finish after the handler returns.
	close(release)
	eventually(t, func() bool { return finishes.Load() == 1 }, "OnAttemptFinish fired once")
	if committed.Load() {
		t.Fatal("abandoned attempt's OnAttemptFinish must have Committed=false")
	}
}

func TestTimeoutIsCooperative(t *testing.T) {
	m := testManager(t, NewMemoryStore())
	started := make(chan struct{})
	release := make(chan struct{})
	Handle[Email](m, "email", func(c Context, e Email) error {
		close(started)
		<-release
		return c.Err()
	})
	runWorker(t, m, WorkerConfig{LeaseDuration: time.Second, HeartbeatInterval: 100 * time.Millisecond})
	id, err := m.Enqueue(context.Background(), Email{}, Timeout(50*time.Millisecond), TimeoutAction(TimeoutFail))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	// Timeout cancels the context but does not terminate the handler.
	time.Sleep(200 * time.Millisecond)
	info, err := m.GetJob(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if info.State != StateRunning {
		t.Fatalf("state = %q while the handler ignores its timeout, want running", info.State)
	}
	// Timeout policy applies after the handler returns.
	close(release)
	eventually(t, func() bool {
		i, _ := m.GetJob(context.Background(), id)
		return i != nil && i.State == StateFailed
	}, "timeout policy applied after return")
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
	_, err := store.Insert(context.Background(), time.Now().UTC(), []Job{{
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
	now := time.Now().UTC()
	res, _ := store.Insert(context.Background(), now, []Job{{
		Kind: "email", HandlerID: "other", Payload: []byte(`{}`),
		Queue: "default", MaxAttempts: 3, AvailableAt: now,
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
