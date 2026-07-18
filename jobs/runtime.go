package jobs

import (
	"context"
	"errors"
	"sync"
)

// RuntimeConfig configures the jobs runtime hosted by [Run].
type RuntimeConfig struct {
	// Worker configures the worker started by [Run].
	Worker WorkerConfig
	// RunScheduler reconciles schedules and starts the cron scheduler.
	// Reconciliation deletes schedules this process does not declare, so enable it only
	// in the process responsible for scheduling.
	RunScheduler bool
}

// Run starts a worker bound to ctx and optionally reconciles schedules and starts the scheduler.
// After ctx is canceled, the returned drain waits using its context, returns the first
// non-cancellation error, and caches the result. Producer-only binaries must not call Run.
func Run(ctx context.Context, m *Manager, cfg RuntimeConfig) (func(context.Context) error, error) {
	w, err := NewWorker(m, cfg.Worker)
	if err != nil {
		return nil, err
	}
	if cfg.RunScheduler {
		if err := m.ReconcileSchedules(ctx); err != nil {
			return nil, err
		}
	}

	workerDone := make(chan error, 1)
	go func() { workerDone <- w.Start(ctx) }()

	var schedulerDone chan error
	if cfg.RunScheduler {
		schedulerDone = make(chan error, 1)
		go func() { schedulerDone <- m.StartScheduler(ctx) }()
	}

	var (
		once   sync.Once
		result error
	)
	drain := func(waitCtx context.Context) error {
		once.Do(func() { result = waitRunnables(waitCtx, workerDone, schedulerDone) })
		return result
	}
	return drain, nil
}

func waitRunnables(waitCtx context.Context, workerDone, schedulerDone <-chan error) error {
	var firstErr error
	for workerDone != nil || schedulerDone != nil {
		select {
		case err := <-workerDone:
			workerDone = nil
			firstErr = keepFirst(firstErr, err)
		case err := <-schedulerDone:
			schedulerDone = nil
			firstErr = keepFirst(firstErr, err)
		case <-waitCtx.Done():
			return waitCtx.Err()
		}
	}
	return firstErr
}

// keepFirst ignores context.Canceled and preserves the first other error.
func keepFirst(prev, err error) error {
	if prev != nil || err == nil || errors.Is(err, context.Canceled) {
		return prev
	}
	return err
}
