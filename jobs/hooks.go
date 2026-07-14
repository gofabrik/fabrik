package jobs

import (
	"context"
	"log/slog"
	"time"
)

// Hooks attach observability without the core depending on any metrics
// or tracing library. Nil fields are skipped. A panicking hook is
// recovered and logged, never taking the worker down; hooks return
// nothing and cannot change an outcome.
type Hooks struct {
	// OnEnqueue fires after a successful insert (for EnqueueTx/PublishTx,
	// after the in-transaction insert but before the caller commits -
	// see Transactional).
	OnEnqueue func(ctx context.Context, e EnqueueEvent)
	// OnAttemptStart fires as a run begins (before the handler, or at
	// the park point when there is no handler).
	OnAttemptStart func(ctx context.Context, e AttemptStartEvent)
	// OnAttemptFinish fires via defer once per run, after Complete, and
	// reports the durable outcome.
	OnAttemptFinish func(ctx context.Context, e AttemptFinishEvent)
}

// EnqueueEvent describes a freshly enqueued job.
type EnqueueEvent struct {
	JobID     string
	Kind      string
	HandlerID string
	// Transactional is true for EnqueueTx/PublishTx, where the hook
	// fires pre-commit; a later rollback leaves the event describing a
	// job that never became durable.
	Transactional bool
	// ScheduleName is non-empty when the job came from a schedule fire.
	ScheduleName string
	// ScheduledFor is the tick of a schedule fire (zero otherwise).
	ScheduledFor time.Time
}

// AttemptStartEvent describes a run about to begin.
type AttemptStartEvent struct {
	JobID     string
	Kind      string
	HandlerID string
	Attempt   int
	Logger    *slog.Logger
}

// AttemptFinishEvent describes a run that just finished.
type AttemptFinishEvent struct {
	JobID     string
	Kind      string
	HandlerID string
	Attempt   int
	// Err is the handler's error (nil on success, a wrapped panic, or a
	// cancellation).
	Err error
	Dur time.Duration
	// State is the durable state the store applied (may differ from the
	// handler's return - the cancel-then-anything rewrite).
	State State
	// Committed is false when Complete could not persist the result (the
	// lease was lost): the run happened but its outcome was abandoned to
	// a re-run.
	Committed bool
}
