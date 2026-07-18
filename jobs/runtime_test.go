package jobs

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func runtimeWorkerConfig() WorkerConfig {
	return WorkerConfig{
		PollInterval:      5 * time.Millisecond,
		LeaseDuration:     time.Second,
		HeartbeatInterval: 100 * time.Millisecond,
		SweepInterval:     time.Second,
	}
}

func TestRun_WorkerDrainsInflightOnCancel(t *testing.T) {
	m := testManager(t, NewMemoryStore())
	started := make(chan struct{})
	release := make(chan struct{})
	var completed atomic.Bool
	Handle[Email](m, "email", func(c Context, e Email) error {
		close(started)
		<-release
		completed.Store(true)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	drain, err := Run(ctx, m, RuntimeConfig{Worker: runtimeWorkerConfig()})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := m.Enqueue(context.Background(), Email{}); err != nil {
		t.Fatal(err)
	}
	<-started
	cancel()

	drainErr := make(chan error, 1)
	go func() { drainErr <- drain(context.Background()) }()
	select {
	case err := <-drainErr:
		t.Fatalf("drain returned before the in-flight job finished: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-drainErr:
		if err != nil {
			t.Fatalf("drain = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("drain did not return after the handler was released")
	}
	if !completed.Load() {
		t.Fatal("an in-flight job must complete during graceful drain, not be abandoned")
	}
}

func TestRun_SchedulerNotHostedSkipsReconcile(t *testing.T) {
	store := NewMemoryStore()
	m := testManager(t, store)
	orphan := ScheduleRow{Group: "", Name: "orphan", Kind: "x", Spec: "every:1h", NextRunAt: time.Now()}
	if err := store.UpsertSchedule(context.Background(), orphan); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	drain, err := Run(ctx, m, RuntimeConfig{Worker: runtimeWorkerConfig(), RunScheduler: false})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	cancel()
	_ = drain(context.Background())

	rows, err := store.ListSchedules(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("orphan schedule must survive when the scheduler is not hosted, got %d rows", len(rows))
	}
}

func TestRun_SchedulerHostedReconciles(t *testing.T) {
	store := NewMemoryStore()
	m := testManager(t, store)
	orphan := ScheduleRow{Group: "", Name: "orphan", Kind: "x", Spec: "every:1h", NextRunAt: time.Now()}
	if err := store.UpsertSchedule(context.Background(), orphan); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	drain, err := Run(ctx, m, RuntimeConfig{Worker: runtimeWorkerConfig(), RunScheduler: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	cancel()
	_ = drain(context.Background())

	rows, err := store.ListSchedules(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("orphan schedule must be pruned when the scheduler is hosted, got %d rows", len(rows))
	}
}

func TestRun_SchedulerRuns(t *testing.T) {
	m := schedManager(t, NewMemoryStore())
	var runs atomic.Int32
	Handle[Email](m, "email", func(Context, Email) error { runs.Add(1); return nil })
	if err := m.Schedule("poll", Every(20*time.Millisecond), Email{To: "x"}, ScheduleOptions{}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	drain, err := Run(ctx, m, RuntimeConfig{Worker: runtimeWorkerConfig(), RunScheduler: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	eventually(t, func() bool { return runs.Load() >= 2 }, "Run hosts the scheduler: it fires repeatedly")
	cancel()
	if err := drain(context.Background()); err != nil {
		t.Fatalf("drain = %v, want nil (the scheduler's context.Canceled must be dropped)", err)
	}
}

func TestRun_DrainHonorsDeadline(t *testing.T) {
	m := testManager(t, NewMemoryStore())
	started := make(chan struct{})
	release := make(chan struct{})
	Handle[Email](m, "email", func(c Context, e Email) error {
		close(started)
		<-release
		return nil
	})

	cfg := runtimeWorkerConfig()
	cfg.ShutdownTimeout = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	drain, err := Run(ctx, m, RuntimeConfig{Worker: cfg})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := m.Enqueue(context.Background(), Email{}); err != nil {
		t.Fatal(err)
	}
	<-started
	cancel()

	waitCtx, wcancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer wcancel()
	start := time.Now()
	if err := drain(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain = %v, want DeadlineExceeded", err)
	}
	elapsed := time.Since(start)
	// The caller's deadline must bound drain well before the worker timeout.
	if elapsed < 40*time.Millisecond {
		t.Fatalf("drain returned after %v; it must wait for the ~50ms deadline, not return instantly", elapsed)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("drain took %v; the outer deadline should fire well before the 1s worker timeout", elapsed)
	}
	if waitCtx.Err() == nil {
		t.Fatal("waitCtx should have reached its deadline")
	}
	close(release)
}

func TestRun_InvalidWorkerConfig(t *testing.T) {
	m := testManager(t, NewMemoryStore())
	drain, err := Run(context.Background(), m, RuntimeConfig{Worker: WorkerConfig{Queues: []string{"bad queue"}}})
	if err == nil {
		t.Fatal("Run must return an error for an invalid worker config")
	}
	if drain != nil {
		t.Fatal("drain must be nil when Run returns an error")
	}
}

func TestKeepFirst(t *testing.T) {
	real := errors.New("boom")
	if got := keepFirst(nil, context.Canceled); got != nil {
		t.Errorf("keepFirst(nil, Canceled) = %v, want nil", got)
	}
	if got := keepFirst(nil, real); got != real {
		t.Errorf("keepFirst(nil, real) = %v, want the error", got)
	}
	if got := keepFirst(real, errors.New("second")); got != real {
		t.Errorf("keepFirst must keep the first error, got %v", got)
	}
	if got := keepFirst(nil, nil); got != nil {
		t.Errorf("keepFirst(nil, nil) = %v, want nil", got)
	}
	if got := keepFirst(nil, context.DeadlineExceeded); got != context.DeadlineExceeded {
		t.Errorf("keepFirst(nil, DeadlineExceeded) = %v, want DeadlineExceeded", got)
	}
}
