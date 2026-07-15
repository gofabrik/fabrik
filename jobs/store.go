package jobs

import (
	"context"
	"database/sql"
	"time"
)

// Store is the persistence contract. Backends must be safe for concurrent
// use; each method is atomic from the caller's view.
type Store interface {
	// Insert writes a batch atomically, stamping created_at/updated_at with
	// now (the manager's clock; stores must not read the wall clock).
	// UniqueKey collisions skip that row with Duplicate=true; other rows
	// still commit.
	Insert(ctx context.Context, now time.Time, jobs []Job) ([]InsertResult, error)

	// Claim locks up to req.Limit eligible rows and returns them as
	// running. Eligible: state in (available, pending) AND
	// AvailableAt <= Now AND queue in req.Queues AND (kind, handler-id)
	// in req.Handlers. Order: priority desc, available_at asc, id asc.
	Claim(ctx context.Context, req ClaimRequest) ([]ClaimedJob, error)

	// Heartbeat extends a running job's lease to until iff this worker
	// still holds it, stamping updated_at with now, and reports whether
	// cancellation was requested. Returns ErrNotFound when the lease was
	// swept or stolen.
	Heartbeat(ctx context.Context, jobID, workerID string, now, until time.Time) (cancelRequested bool, err error)

	// Complete persists the attempt and outcome only while workerID owns the lease.
	// Cancellation wins, and now cannot regress updated_at.
	Complete(ctx context.Context, jobID, workerID string, now time.Time, o Outcome) (applied State, err error)

	// SweepExpired records expired leases as attempts and applies retry exhaustion.
	SweepExpired(ctx context.Context, now time.Time) (int, error)

	// Get returns a job by id, or ErrNotFound.
	Get(ctx context.Context, id string) (*JobInfo, error)

	// List returns a page of jobs matching the filter, with an opaque
	// next cursor (empty when exhausted).
	List(ctx context.Context, f ListFilter) (rows []JobInfo, nextCursor string, err error)

	// ListAttempts returns up to limit attempts for jobID whose number
	// is strictly greater than afterAttempt, ascending.
	ListAttempts(ctx context.Context, jobID string, afterAttempt, limit int) ([]Attempt, error)

	// Retry revives a terminal job (failed, discarded, cancelled) to
	// available, re-checking UniqueKey. Returns ErrJobNotRetryable for a
	// non-revivable state, or *DuplicateError on a live-key collision.
	Retry(ctx context.Context, jobID string, now time.Time) error

	// Cancel cancels a job: pending/available -> cancelled now
	// (immediate=true); running -> flag set (immediate=false); terminal
	// -> ErrJobTerminal.
	Cancel(ctx context.Context, jobID string, now time.Time) (immediate bool, err error)

	// Delete removes a job and its attempts. Returns ErrJobRunning for a
	// leased job.
	Delete(ctx context.Context, jobID string) error

	// UpsertWorker registers or refreshes a worker row.
	UpsertWorker(ctx context.Context, w WorkerRow) error
	// RetireWorker removes a worker row.
	RetireWorker(ctx context.Context, workerID string) error
	// ListWorkers returns alive workers, ordered by StartedAt.
	ListWorkers(ctx context.Context) ([]WorkerRow, error)
	// SweepStaleWorkers removes workers whose LastSeenAt is older than
	// the cutoff. Returns the number removed.
	SweepStaleWorkers(ctx context.Context, olderThan time.Time) (int, error)

	// ListQueues returns one entry per distinct queue, counts per state,
	// ordered by name.
	ListQueues(ctx context.Context) ([]QueueInfo, error)

	// UpsertSchedule inserts or updates by (Group, Name). Existing rows
	// preserve LastRunAt and keep NextRunAt unless the Spec changed.
	UpsertSchedule(ctx context.Context, s ScheduleRow) error
	// DeleteSchedule removes a schedule by (group, name). No-op absent.
	DeleteSchedule(ctx context.Context, group, name string) error
	// ListSchedules returns every schedule in a group, ordered by Name.
	ListSchedules(ctx context.Context, group string) ([]ScheduleRow, error)
	// DueSchedules returns schedules in a group whose NextRunAt <= now.
	DueSchedules(ctx context.Context, group string, now time.Time) ([]ScheduleRow, error)
	// FireSchedule advances a schedule and inserts its jobs in one
	// transaction iff LastRunAt still matches ExpectedLastRun.
	FireSchedule(ctx context.Context, f ScheduleFire) (won bool, results []InsertResult, err error)
}

// TxEnqueuer is the optional transactional-enqueue capability.
type TxEnqueuer interface {
	InsertTx(ctx context.Context, tx *sql.Tx, now time.Time, jobs []Job) ([]InsertResult, error)
}

// Job is an insert row; the store assigns its ID and timestamps.
type Job struct {
	Kind        string
	HandlerID   string
	Payload     []byte
	Queue       string
	Priority    int
	AvailableAt time.Time
	MaxAttempts int
	// TimeoutMs is the per-attempt timeout in milliseconds (0 = none).
	TimeoutMs int64
	OnTimeout OnTimeout
	// BackoffSpec is the JSON of a serializable per-job backoff, or nil
	// to use the manager default.
	BackoffSpec []byte
	UniqueKey   string
	// ScheduledFor is the tick a scheduler fire represents; zero (and
	// ScheduledForSet=false) for directly enqueued jobs.
	ScheduledFor    time.Time
	ScheduledForSet bool
}

// InsertResult is the per-row outcome of [Store.Insert], in input order.
type InsertResult struct {
	ID        string
	CreatedAt time.Time
	Kind      string
	HandlerID string
	// ScheduledFor preserves each schedule tick across catch-up inserts.
	ScheduledFor time.Time
	// Duplicate is true when a UniqueKey collision made the store skip
	// the insert; ID is then the existing live job's id.
	Duplicate bool
}

// ClaimRequest is the parameter to [Store.Claim].
type ClaimRequest struct {
	WorkerID string
	Queues   []string
	Now      time.Time
	Lease    time.Duration
	Limit    int
	// QueueLimits caps how many rows this claim may take per queue (the
	// worker's PerQueue budget minus in-flight). Absent = unlimited.
	QueueLimits map[string]int
	// Handlers is the worker's registered (kind, handler-id) set. The
	// store returns only rows whose pair is present.
	Handlers map[HandlerKey]struct{}
}

// HandlerKey is the persisted routing identity of a registered handler.
type HandlerKey struct {
	Kind      string
	HandlerID string
}

// ClaimedJob is a [Job] plus its lease identity.
type ClaimedJob struct {
	Job
	ID          string
	Attempt     int
	LockedUntil time.Time
}

// Outcome contains the job state and attempt ledger fields written by [Store.Complete].
type Outcome struct {
	State       State
	Attempt     int
	Error       string
	AvailableAt time.Time // retry time when State is pending

	AttemptState AttemptState
	StartedAt    time.Time
	FinishedAt   time.Time
}

// ListFilter narrows [Store.List]. The zero value matches everything.
type ListFilter struct {
	Queues []string
	Kinds  []string
	States []State
	Limit  int
	Cursor string
}

// Attempt is one row of the job_attempts ledger.
type Attempt struct {
	ID         string
	JobID      string
	Attempt    int
	WorkerID   string
	State      AttemptState
	Error      string
	StartedAt  time.Time
	FinishedAt time.Time
}

// WorkerRow is the persistence shape of a worker registration.
type WorkerRow struct {
	ID         string
	Hostname   string
	Queues     []string
	StartedAt  time.Time
	LastSeenAt time.Time
}

// QueueInfo describes one queue and its per-state counts.
type QueueInfo struct {
	Name   string
	Counts map[State]int
}

// ScheduleRow is the persistence shape of a schedule.
type ScheduleRow struct {
	Group       string
	Name        string
	Kind        string
	Spec        string // the spec source ("cron:..." / "every:...")
	Payload     []byte
	OptionsJSON []byte
	NextRunAt   time.Time
	LastRunAt   time.Time // zero when never fired
	LastRunSet  bool      // false = never fired (NULL last_run_at)
	UpdatedAt   time.Time
}

// ScheduleFire is the parameter to [Store.FireSchedule].
type ScheduleFire struct {
	Group           string
	Name            string
	ExpectedLastRun sql.NullTime // Valid=false for the first fire
	NewLastRun      time.Time
	NewNextRun      time.Time
	Now             time.Time // manager clock: schedule updated_at and inserted jobs' timestamps
	Jobs            []Job
}
