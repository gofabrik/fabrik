package jobs

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunner_DrainsInflightBeforeReturning(t *testing.T) {
	m := testManager(t, NewMemoryStore())
	started := make(chan struct{})
	release := make(chan struct{})
	var completed atomic.Bool
	Handle[Email](m, "email", func(Context, Email) error {
		close(started)
		<-release
		completed.Store(true)
		return nil
	})
	if _, err := m.Enqueue(context.Background(), Email{}); err != nil {
		t.Fatal(err)
	}

	r := NewRunner(m, RuntimeConfig{Worker: runtimeWorkerConfig()})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- r.Run(ctx) }()

	<-started
	cancel()
	select {
	case err := <-errc:
		t.Fatalf("Run returned before the in-flight job finished: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("Run = %v, want nil on clean shutdown", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after the handler was released")
	}
	if !completed.Load() {
		t.Fatal("an in-flight job must complete during drain, not be abandoned")
	}
}

func TestRunner_ReturnsInitError(t *testing.T) {
	m := testManager(t, NewMemoryStore())
	r := NewRunner(m, RuntimeConfig{Worker: WorkerConfig{Queues: []string{"bad queue"}}})
	if err := r.Run(context.Background()); err == nil {
		t.Fatal("Run must return the init error when the worker config is invalid")
	}
}
