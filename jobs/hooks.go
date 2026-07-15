package jobs

import (
	"context"
	"log/slog"
	"time"
)

// Hooks observe enqueue and attempt events without changing their outcomes.
// Panics are recovered and logged.
type Hooks struct {
	// OnEnqueue fires after insertion. Transactional inserts fire before commit.
	OnEnqueue func(ctx context.Context, e EnqueueEvent)
	// OnAttemptStart fires before handler invocation or terminal parking.
	OnAttemptStart func(ctx context.Context, e AttemptStartEvent)
	// OnAttemptFinish fires once per run; Committed reports whether its outcome persisted.
	OnAttemptFinish func(ctx context.Context, e AttemptFinishEvent)
}

// EnqueueEvent describes a freshly enqueued job.
type EnqueueEvent struct {
	JobID     string
	Kind      string
	HandlerID string
	// Transactional marks pre-commit EnqueueTx and PublishTx events.
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

// AttemptFinishEvent describes a completed handler run.
type AttemptFinishEvent struct {
	JobID     string
	Kind      string
	HandlerID string
	Attempt   int
	// Err is the handler error, panic, or cancellation.
	Err error
	Dur time.Duration
	// State is the persisted outcome and may differ from the handler result.
	State State
	// Committed is false when the outcome was abandoned or ownership was lost.
	Committed bool
}
