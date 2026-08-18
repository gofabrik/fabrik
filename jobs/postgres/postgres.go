// Package postgres implements a jobs.Store backed by PostgreSQL.
package postgres

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"time"

	"github.com/gofabrik/fabrik/jobs"
	"github.com/gofabrik/fabrik/jobs/internal/sqlstore"
)

// Times use Unix nanoseconds; nullable timestamps are NULL when unset.
const schema = `
CREATE TABLE IF NOT EXISTS jobs (
    id               TEXT    PRIMARY KEY,
    kind             TEXT    NOT NULL,
    handler_id       TEXT    NOT NULL,
    payload          BYTEA   NOT NULL,
    queue            TEXT    NOT NULL,
    priority         INTEGER NOT NULL DEFAULT 0,
    state            TEXT    NOT NULL,
    attempt          INTEGER NOT NULL DEFAULT 0,
    max_attempts     INTEGER NOT NULL,
    available_at     BIGINT  NOT NULL,
    timeout_ms       BIGINT  NOT NULL DEFAULT 0,
    on_timeout       INTEGER NOT NULL DEFAULT 0,
    backoff_spec     BYTEA,
    unique_key       BYTEA   NOT NULL DEFAULT '',
    scheduled_for    BIGINT,
    error            TEXT    NOT NULL DEFAULT '',
    locked_by        TEXT    NOT NULL DEFAULT '',
    locked_until     BIGINT  NOT NULL DEFAULT 0,
    cancel_requested INTEGER NOT NULL DEFAULT 0,
    created_at       BIGINT  NOT NULL,
    updated_at       BIGINT  NOT NULL
);
CREATE INDEX IF NOT EXISTS jobs_claim ON jobs(queue, state, available_at);
CREATE INDEX IF NOT EXISTS jobs_list ON jobs(created_at, id);
CREATE UNIQUE INDEX IF NOT EXISTS jobs_unique ON jobs(kind, handler_id, unique_key)
    WHERE unique_key <> '' AND state NOT IN ('succeeded','failed','cancelled','discarded');

CREATE TABLE IF NOT EXISTS job_attempts (
    id          TEXT    PRIMARY KEY,
    job_id      TEXT    NOT NULL,
    attempt     INTEGER NOT NULL,
    worker_id   TEXT    NOT NULL,
    state       TEXT    NOT NULL,
    error       TEXT    NOT NULL DEFAULT '',
    started_at  BIGINT  NOT NULL,
    finished_at BIGINT  NOT NULL
);
CREATE INDEX IF NOT EXISTS job_attempts_job ON job_attempts(job_id, attempt);

CREATE TABLE IF NOT EXISTS job_workers (
    id           TEXT   PRIMARY KEY,
    hostname     TEXT   NOT NULL DEFAULT '',
    queues       TEXT   NOT NULL DEFAULT '',
    started_at   BIGINT NOT NULL,
    last_seen_at BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS job_schedules (
    sched_group  TEXT   NOT NULL,
    name         TEXT   NOT NULL,
    kind         TEXT   NOT NULL,
    spec         TEXT   NOT NULL,
    payload      BYTEA  NOT NULL,
    options_json BYTEA  NOT NULL,
    next_run_at  BIGINT NOT NULL,
    last_run_at  BIGINT,
    updated_at   BIGINT NOT NULL,
    PRIMARY KEY (sched_group, name)
);`

// Schema returns the idempotent DDL backing the store.
func Schema() string { return schema }

// Options configures New.
type Options struct {
	// AutoCreate applies Schema during New.
	AutoCreate bool
}

// Store is a durable jobs.Store backed by a caller-opened database.
type Store struct {
	eng *sqlstore.Engine
}

// New constructs a store over db.
func New(db *sql.DB, opts Options) (*Store, error) {
	eng, err := sqlstore.New(db, dialect{}, opts.AutoCreate)
	if err != nil {
		return nil, err
	}
	return &Store{eng: eng}, nil
}

type dialect struct{}

// Rebind rewrites ? placeholders to $1..$n, leaving quoted literals
// untouched.
func (dialect) Rebind(q string) string {
	var b strings.Builder
	b.Grow(len(q) + 8)
	n := 0
	inQuote := false
	for i := 0; i < len(q); i++ {
		c := q[i]
		switch {
		case c == '\'':
			inQuote = !inQuote
			b.WriteByte(c)
		case c == '?' && !inQuote:
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
func (dialect) Schema() string { return schema }
func (dialect) InsertConflictClause() string {
	return " ON CONFLICT DO NOTHING"
}

func (dialect) WorkerUpsertSQL() string {
	return `INSERT INTO job_workers (id,hostname,queues,started_at,last_seen_at)
VALUES (?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET hostname=excluded.hostname, queues=excluded.queues, last_seen_at=excluded.last_seen_at`
}
func (dialect) NullSafeEq(col string) string { return col + " IS NOT DISTINCT FROM ?" }

// ByteArg preserves arbitrary unique keys in the BYTEA column.
func (dialect) ByteArg(s string) any        { return []byte(s) }
func (dialect) ForUpdate() string           { return " FOR UPDATE" }
func (dialect) Greatest(a, b string) string { return "GREATEST(" + a + ", " + b + ")" }

func (s *Store) Insert(ctx context.Context, now time.Time, js []jobs.Job) ([]jobs.InsertResult, error) {
	return s.eng.Insert(ctx, now, js)
}

func (s *Store) InsertTx(ctx context.Context, tx *sql.Tx, now time.Time, js []jobs.Job) ([]jobs.InsertResult, error) {
	return s.eng.InsertTx(ctx, tx, now, js)
}

func (s *Store) Claim(ctx context.Context, req jobs.ClaimRequest) ([]jobs.ClaimedJob, error) {
	return s.eng.Claim(ctx, req)
}

func (s *Store) Heartbeat(ctx context.Context, jobID, workerID string, now, until time.Time) (bool, error) {
	return s.eng.Heartbeat(ctx, jobID, workerID, now, until)
}

func (s *Store) Complete(ctx context.Context, jobID, workerID string, now time.Time, o jobs.Outcome) (jobs.State, error) {
	return s.eng.Complete(ctx, jobID, workerID, now, o)
}

func (s *Store) SweepExpired(ctx context.Context, now time.Time) (int, error) {
	return s.eng.SweepExpired(ctx, now)
}

func (s *Store) Get(ctx context.Context, id string) (*jobs.JobInfo, error) {
	return s.eng.Get(ctx, id)
}

func (s *Store) List(ctx context.Context, f jobs.ListFilter) ([]jobs.JobInfo, string, error) {
	return s.eng.List(ctx, f)
}

func (s *Store) ListAttempts(ctx context.Context, jobID string, afterAttempt, limit int) ([]jobs.Attempt, error) {
	return s.eng.ListAttempts(ctx, jobID, afterAttempt, limit)
}

func (s *Store) Retry(ctx context.Context, jobID string, now time.Time) error {
	return s.eng.Retry(ctx, jobID, now)
}

func (s *Store) Cancel(ctx context.Context, jobID string, now time.Time) (bool, error) {
	return s.eng.Cancel(ctx, jobID, now)
}

func (s *Store) Delete(ctx context.Context, jobID string) error {
	return s.eng.Delete(ctx, jobID)
}

func (s *Store) UpsertWorker(ctx context.Context, w jobs.WorkerRow) error {
	return s.eng.UpsertWorker(ctx, w)
}

func (s *Store) RetireWorker(ctx context.Context, workerID string) error {
	return s.eng.RetireWorker(ctx, workerID)
}

func (s *Store) ListWorkers(ctx context.Context) ([]jobs.WorkerRow, error) {
	return s.eng.ListWorkers(ctx)
}

func (s *Store) SweepStaleWorkers(ctx context.Context, olderThan time.Time) (int, error) {
	return s.eng.SweepStaleWorkers(ctx, olderThan)
}

func (s *Store) ListQueues(ctx context.Context) ([]jobs.QueueInfo, error) {
	return s.eng.ListQueues(ctx)
}

func (s *Store) UpsertSchedule(ctx context.Context, row jobs.ScheduleRow) error {
	return s.eng.UpsertSchedule(ctx, row)
}

func (s *Store) DeleteSchedule(ctx context.Context, group, name string) error {
	return s.eng.DeleteSchedule(ctx, group, name)
}

func (s *Store) ListSchedules(ctx context.Context, group string) ([]jobs.ScheduleRow, error) {
	return s.eng.ListSchedules(ctx, group)
}

func (s *Store) DueSchedules(ctx context.Context, group string, now time.Time) ([]jobs.ScheduleRow, error) {
	return s.eng.DueSchedules(ctx, group, now)
}

func (s *Store) FireSchedule(ctx context.Context, f jobs.ScheduleFire) (bool, []jobs.InsertResult, error) {
	return s.eng.FireSchedule(ctx, f)
}

var (
	_ jobs.Store      = (*Store)(nil)
	_ jobs.TxEnqueuer = (*Store)(nil)
)
