// Package jobs is a durable, embeddable background-job engine for Go.
//
// A message is a plain JSON-serializable struct (pure data); a handler
// is a function registered against a message type with [On] (or the
// [Handle] command shortcut). One handler on a type is a command,
// enqueued with [Manager.Enqueue]; many handlers on a type are an event,
// fanned out with [Manager.Publish]. Both persist as jobs keyed by
// (kind, handler-id) and use the same retry, lease, and inspection machinery.
//
// Jobs are at-least-once: a worker can crash mid-attempt and the job
// runs again, so non-idempotent side effects should carry their own
// idempotency key. Two backends ship in this package: an in-memory store
// (tests, examples, local dev) and a SQLite store (single node,
// durable). Neither imports a SQL driver; the SQLite store takes a
// caller-opened *sql.DB.
//
// In a fabrik app, the //fabrik:job directive generates registration code.
package jobs

import (
	crand "crypto/rand"
	"errors"
	"fmt"
)

// State is the lifecycle position of a job. The zero value is not a
// valid state; every persisted row carries one of the constants below.
type State string

const (
	// StateAvailable: claimable now.
	StateAvailable State = "available"
	// StatePending: enqueued but not yet due.
	StatePending State = "pending"
	// StateRunning: leased by a worker and executing.
	StateRunning State = "running"
	// StateSucceeded: the handler returned nil. Terminal.
	StateSucceeded State = "succeeded"
	// StateFailed: a non-retryable failure or an exhausted error.
	// Terminal; revivable with [Manager.RetryJob].
	StateFailed State = "failed"
	// StateCancelled: cancelled by an operator. Terminal.
	StateCancelled State = "cancelled"
	// StateDiscarded: retries exhausted. Terminal.
	StateDiscarded State = "discarded"
)

// Terminal reports whether workers can no longer claim s.
func (s State) Terminal() bool {
	switch s {
	case StateSucceeded, StateFailed, StateCancelled, StateDiscarded:
		return true
	}
	return false
}

// AttemptState records how a single run of a job ended. It is distinct
// from [State]: an attempt's state is the cause of one run's outcome,
// the job's state is where the job now sits. A per-attempt timeout is
// [AttemptTimedOut] whether the job then retries, fails, or discards.
type AttemptState string

const (
	AttemptSucceeded AttemptState = "succeeded"
	AttemptFailed    AttemptState = "failed"
	AttemptTimedOut  AttemptState = "timed_out"
	AttemptCancelled AttemptState = "cancelled"
)

// OnTimeout decides what a timed-out attempt does. The zero value is
// [TimeoutRetry].
type OnTimeout int

const (
	// TimeoutRetry retries the attempt (subject to the retry cap).
	TimeoutRetry OnTimeout = iota
	// TimeoutFail moves the job to failed immediately.
	TimeoutFail
	// TimeoutDiscard moves the job to discarded immediately.
	TimeoutDiscard
)

// CatchUp decides how a schedule handles ticks missed during downtime.
// The zero value is [CatchUpOnce].
type CatchUp int

const (
	// CatchUpOnce fires one job if any tick was missed, then resumes.
	CatchUpOnce CatchUp = iota
	// CatchUpSkip never fires missed ticks.
	CatchUpSkip
	// CatchUpAll fires one job per missed tick, capped at
	// [CatchUpAllMax], then skips the rest of the backlog.
	CatchUpAll
)

// Sentinel errors. Wrap with fmt.Errorf("...: %w", jobs.ErrX) when
// adding context; match with errors.Is.
var (
	// ErrPermanent, returned or wrapped from a handler, moves the job
	// straight to [StateFailed] regardless of remaining attempts.
	ErrPermanent = errors.New("permanent failure, do not retry")

	// ErrUnregistered is returned by [Manager.Enqueue]/[Manager.Publish]
	// when the message type has no registration in this process.
	ErrUnregistered = errors.New("message type is not registered")

	// ErrUnregisteredHandler is recorded when a claimed job names a
	// (kind, handler-id) this worker does not have. In practice the
	// claim filter prevents it; it is the defensive sentinel.
	ErrUnregisteredHandler = errors.New("handler is not registered")

	// ErrDecodePayload is recorded when a job's persisted payload no
	// longer decodes into its message type. Terminal park.
	ErrDecodePayload = errors.New("job payload cannot be decoded")

	// ErrBackoffNotSerializable is returned by enqueue when a per-job
	// Backoff override is not a serializable spec (only
	// [ExponentialBackoff] round-trips).
	ErrBackoffNotSerializable = errors.New("per-job Backoff must be a serializable spec (ExponentialBackoff)")

	// ErrDuplicate is returned when a UniqueKey collides with a live
	// job of the same (kind, handler-id). The existing id travels on a
	// [*DuplicateError].
	ErrDuplicate = errors.New("duplicate unique key")

	// ErrNotFound is returned when a lookup by id resolves to no row,
	// and by the lease methods when ownership was lost.
	ErrNotFound = errors.New("not found")

	// ErrUnsupported is returned by store operations a backend cannot
	// implement (transactional enqueue against the memory store).
	ErrUnsupported = errors.New("operation not supported by store")

	// ErrJobRunning is returned by [Manager.DeleteJob] for a leased job.
	ErrJobRunning = errors.New("job is running")

	// ErrJobTerminal is returned by [Manager.CancelJob] for a job that
	// is already terminal.
	ErrJobTerminal = errors.New("job is in a terminal state")

	// ErrJobNotRetryable is returned by [Manager.RetryJob] for a job
	// that is not in a revivable terminal state.
	ErrJobNotRetryable = errors.New("job is not retryable")

	// ErrTypeAlreadyRegistered is returned by [Register] when the Go
	// type already has a wire kind.
	ErrTypeAlreadyRegistered = errors.New("message type already registered")

	// ErrKindAlreadyRegistered is returned by [Register] when the kind
	// is already bound to a different type.
	ErrKindAlreadyRegistered = errors.New("kind already registered")

	// ErrHandlerAlreadyRegistered is returned by [On] when a handler-id
	// is already present for a kind.
	ErrHandlerAlreadyRegistered = errors.New("handler-id already registered for kind")

	// ErrScheduleAlreadyDeclared is returned when a schedule name is
	// already declared in this process.
	ErrScheduleAlreadyDeclared = errors.New("schedule name already declared")

	// ErrWorkerAlreadyStarted is returned by [Worker.Start] when the worker
	// has already been started. A worker is single-use; construct a new one.
	ErrWorkerAlreadyStarted = errors.New("worker already started")
)

// DuplicateError carries the id of the live job that already holds a
// UniqueKey. errors.Is(err, [ErrDuplicate]) is true.
type DuplicateError struct {
	ExistingID string
	Kind       string
	HandlerID  string
	UniqueKey  string
}

func (e *DuplicateError) Error() string {
	return fmt.Sprintf("duplicate unique key %q for %s/%s (existing job %s)",
		e.UniqueKey, e.Kind, e.HandlerID, e.ExistingID)
}

func (e *DuplicateError) Is(target error) bool { return target == ErrDuplicate }

// NewID returns a UUIDv4 string for backend implementors.
func NewID() string {
	var b [16]byte
	if _, err := crand.Read(b[:]); err != nil {
		// Failure indicates an unusable system random source.
		panic("jobs: crypto/rand.Read failed: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant RFC 4122
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// validIdent enforces the identifier format shared by kind, handler-id,
// queue, and schedule name.
func validIdent(what, s string) error {
	if s == "" {
		return fmt.Errorf("jobs: %s is empty", what)
	}
	if len(s) > 255 {
		return fmt.Errorf("jobs: %s %q exceeds 255 bytes", what, s)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == ':' || c == '/' || c == '-':
		default:
			return fmt.Errorf("jobs: %s %q has invalid byte %q (allowed: A-Za-z0-9._:/-)", what, s, string(c))
		}
	}
	return nil
}

func validOnTimeout(o OnTimeout) error {
	switch o {
	case TimeoutRetry, TimeoutFail, TimeoutDiscard:
		return nil
	}
	return fmt.Errorf("jobs: OnTimeout has an out-of-range value (%d)", int(o))
}

func validCatchUp(c CatchUp) error {
	switch c {
	case CatchUpOnce, CatchUpSkip, CatchUpAll:
		return nil
	}
	return fmt.Errorf("jobs: CatchUp has an out-of-range value (%d)", int(c))
}
