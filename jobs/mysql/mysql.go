// Package mysql implements a jobs.Store backed by MySQL or MariaDB.
//
// Identifiers compare byte-for-byte and are limited to 255 bytes;
// unique keys are limited to 2048 bytes. A generated column provides
// active-job uniqueness because MySQL has no partial indexes. Guarded
// writes lock rows because unchanged matches and misses both report zero.
package mysql

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/gofabrik/fabrik/jobs"
	"github.com/gofabrik/fabrik/jobs/internal/sqlstore"
)

const schema = `
CREATE TABLE IF NOT EXISTS jobs (
    id               VARBINARY(191)  NOT NULL,
    kind             VARBINARY(255)  NOT NULL,
    handler_id       VARBINARY(255)  NOT NULL,
    payload          LONGBLOB        NOT NULL,
    queue            VARBINARY(255)  NOT NULL,
    priority         INTEGER         NOT NULL DEFAULT 0,
    state            VARBINARY(16)   NOT NULL,
    attempt          INTEGER         NOT NULL DEFAULT 0,
    max_attempts     INTEGER         NOT NULL,
    available_at     BIGINT          NOT NULL,
    timeout_ms       BIGINT          NOT NULL DEFAULT 0,
    on_timeout       INTEGER         NOT NULL DEFAULT 0,
    backoff_spec     LONGBLOB,
    unique_key       VARBINARY(2048) NOT NULL DEFAULT '',
    scheduled_for    BIGINT,
    error            TEXT            NOT NULL,
    locked_by        VARBINARY(255)  NOT NULL DEFAULT '',
    locked_until     BIGINT          NOT NULL DEFAULT 0,
    cancel_requested INTEGER         NOT NULL DEFAULT 0,
    created_at       BIGINT          NOT NULL,
    updated_at       BIGINT          NOT NULL,
    active           TINYINT GENERATED ALWAYS AS
        (CASE WHEN unique_key <> '' AND state NOT IN ('succeeded','failed','cancelled','discarded') THEN 1 ELSE NULL END) STORED,
    PRIMARY KEY (id),
    INDEX jobs_claim (queue, state, available_at),
    INDEX jobs_list (created_at, id),
    UNIQUE KEY jobs_unique (kind, handler_id, unique_key, active)
);

CREATE TABLE IF NOT EXISTS job_attempts (
    id          VARBINARY(191) NOT NULL,
    job_id      VARBINARY(191) NOT NULL,
    attempt     INTEGER        NOT NULL,
    worker_id   VARBINARY(255) NOT NULL,
    state       VARBINARY(32)  NOT NULL,
    error       TEXT           NOT NULL,
    started_at  BIGINT         NOT NULL,
    finished_at BIGINT         NOT NULL,
    PRIMARY KEY (id),
    INDEX job_attempts_job (job_id, attempt)
);

CREATE TABLE IF NOT EXISTS job_workers (
    id           VARBINARY(255) NOT NULL,
    hostname     TEXT           NOT NULL,
    queues       TEXT           NOT NULL,
    started_at   BIGINT         NOT NULL,
    last_seen_at BIGINT         NOT NULL,
    PRIMARY KEY (id)
);

CREATE TABLE IF NOT EXISTS job_schedules (
    sched_group  VARBINARY(255) NOT NULL,
    name         VARBINARY(255) NOT NULL,
    kind         VARBINARY(255) NOT NULL,
    spec         TEXT           NOT NULL,
    payload      LONGBLOB       NOT NULL,
    options_json LONGBLOB       NOT NULL,
    next_run_at  BIGINT         NOT NULL,
    last_run_at  BIGINT,
    updated_at   BIGINT         NOT NULL,
    PRIMARY KEY (sched_group, name)
);`

// Schema returns idempotent DDL. Direct application requires multiStatements=true.
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

func (dialect) Rebind(q string) string { return q }
func (dialect) Schema() string         { return schema }

// InsertConflictClause avoids row counts affected by clientFoundRows.
func (dialect) InsertConflictClause() string { return "" }

// IsDuplicateErr recognizes error 1062 without importing a SQL driver.
func (dialect) IsDuplicateErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Error 1062")
}

func (dialect) WorkerUpsertSQL() string {
	return `INSERT INTO job_workers (id,hostname,queues,started_at,last_seen_at)
VALUES (?,?,?,?,?)
ON DUPLICATE KEY UPDATE hostname=VALUES(hostname), queues=VALUES(queues), last_seen_at=VALUES(last_seen_at)`
}
func (dialect) NullSafeEq(col string) string { return col + " <=> ?" }
func (dialect) ForUpdate() string            { return " FOR UPDATE" }

// ShareRead uses syntax shared by MySQL and MariaDB without upgrading locks.
func (dialect) ShareRead() string           { return " LOCK IN SHARE MODE" }
func (dialect) Greatest(a, b string) string { return "GREATEST(" + a + ", " + b + ")" }

// Heartbeat locks the lease so an unchanged extension reports success.
func (dialect) Heartbeat(ctx context.Context, db *sql.DB, jobID, workerID string, now, until time.Time) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck // rollback is best-effort cleanup after commit or an earlier error

	var cancelReq int
	err = tx.QueryRowContext(ctx,
		"SELECT cancel_requested FROM jobs WHERE id=? AND state='running' AND locked_by=? FOR UPDATE",
		jobID, workerID).Scan(&cancelReq)
	if err == sql.ErrNoRows {
		return false, jobs.ErrNotFound
	}
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE jobs SET locked_until=?, updated_at=? WHERE id=?",
		until.UTC().UnixNano(), now.UTC().UnixNano(), jobID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return cancelReq != 0, nil
}

// Cancel locks the job so repeated cancellation remains successful.
func (dialect) Cancel(ctx context.Context, db *sql.DB, jobID string, now time.Time) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck // rollback is best-effort cleanup after commit or an earlier error

	var state string
	var cancelReq int
	err = tx.QueryRowContext(ctx, "SELECT state, cancel_requested FROM jobs WHERE id=? FOR UPDATE", jobID).
		Scan(&state, &cancelReq)
	if err == sql.ErrNoRows {
		return false, jobs.ErrNotFound
	}
	if err != nil {
		return false, err
	}
	nowN := now.UTC().UnixNano()
	switch jobs.State(state) {
	case jobs.StatePending, jobs.StateAvailable:
		if _, err := tx.ExecContext(ctx,
			"UPDATE jobs SET state='cancelled', updated_at=? WHERE id=?", nowN, jobID); err != nil {
			return false, err
		}
		return true, tx.Commit()
	case jobs.StateRunning:
		// Record when cancellation was last requested.
		if _, err := tx.ExecContext(ctx,
			"UPDATE jobs SET cancel_requested=1, updated_at=? WHERE id=?", nowN, jobID); err != nil {
			return false, err
		}
		return false, tx.Commit()
	default:
		return false, jobs.ErrJobTerminal
	}
}

// FireCAS locks the schedule so an unchanged election still wins.
func (dialect) FireCAS(ctx context.Context, tx *sql.Tx, f jobs.ScheduleFire) (bool, error) {
	var last sql.NullInt64
	err := tx.QueryRowContext(ctx,
		"SELECT last_run_at FROM job_schedules WHERE sched_group=? AND name=? FOR UPDATE",
		f.Group, f.Name).Scan(&last)
	if err == sql.ErrNoRows {
		return false, jobs.ErrNotFound
	}
	if err != nil {
		return false, err
	}
	if last.Valid != f.ExpectedLastRun.Valid {
		return false, nil
	}
	if last.Valid && last.Int64 != f.ExpectedLastRun.Time.UTC().UnixNano() {
		return false, nil
	}
	var newLast any
	if !f.NewLastRun.IsZero() {
		newLast = f.NewLastRun.UTC().UnixNano()
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE job_schedules SET last_run_at=?, next_run_at=?, updated_at=? WHERE sched_group=? AND name=?",
		newLast, f.NewNextRun.UTC().UnixNano(), f.Now.UTC().UnixNano(), f.Group, f.Name); err != nil {
		return false, err
	}
	return true, nil
}

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

// TxOptions exposes committed winners without upgrading shared locks.
func (dialect) TxOptions() *sql.TxOptions {
	return &sql.TxOptions{Isolation: sql.LevelReadCommitted}
}
