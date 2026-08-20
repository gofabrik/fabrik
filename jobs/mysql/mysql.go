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
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofabrik/fabrik/jobs"
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
	db *sql.DB
}

// New constructs a store over db.
func New(db *sql.DB, opts Options) (*Store, error) {
	if db == nil {
		return nil, errJobs("nil db")
	}
	if opts.AutoCreate {
		if _, err := db.ExecContext(context.Background(), schema); err != nil {
			return nil, errJobsf("create schema: %w", err)
		}
	}
	return &Store{db: db}, nil
}

func errJobs(msg string) error { return errors.New("jobs: " + msg) }

func errJobsf(format string, a ...any) error { return fmt.Errorf("jobs: "+format, a...) }

func nanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().UnixNano()
}

func fromNanos(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

func nullNanos(t time.Time, set bool) sql.NullInt64 {
	if !set || t.IsZero() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.UTC().UnixNano(), Valid: true}
}

const terminalStates = "'succeeded','failed','cancelled','discarded'"

var readCommitted = &sql.TxOptions{Isolation: sql.LevelReadCommitted}

// isDuplicateErr recognizes error 1062 without importing a SQL driver.
func isDuplicateErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Error 1062")
}

func (s *Store) Insert(ctx context.Context, now time.Time, js []jobs.Job) ([]jobs.InsertResult, error) {
	tx, err := s.db.BeginTx(ctx, readCommitted)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // rollback is best-effort cleanup after commit or an earlier error
	out, err := s.insertTx(ctx, tx, now, js)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// InsertTx implements jobs.TxEnqueuer.
func (s *Store) InsertTx(ctx context.Context, tx *sql.Tx, now time.Time, js []jobs.Job) ([]jobs.InsertResult, error) {
	return s.insertTx(ctx, tx, now, js)
}

// insertTx writes a batch while deduplicating live unique keys.
func (s *Store) insertTx(ctx context.Context, tx *sql.Tx, now time.Time, js []jobs.Job) ([]jobs.InsertResult, error) {
	now = now.UTC()
	const liveHolder = "SELECT id FROM jobs WHERE kind=? AND handler_id=? AND unique_key=? AND state NOT IN (" + terminalStates + ") LIMIT 1"
	// MySQL uses a shared lock to avoid upgrading the failed insert's lock.
	const conflictHolder = "SELECT id FROM jobs WHERE kind=? AND handler_id=? AND unique_key=? AND state NOT IN (" + terminalStates + ") LIMIT 1 LOCK IN SHARE MODE"
	out := make([]jobs.InsertResult, 0, len(js))
	for _, j := range js {
		res, err := s.insertOne(ctx, tx, now, j, liveHolder, conflictHolder)
		if err != nil {
			return nil, err
		}
		out = append(out, res)
	}
	return out, nil
}

// insertOne retries when a conflicting holder vanishes before lookup.
func (s *Store) insertOne(ctx context.Context, tx *sql.Tx, now time.Time, j jobs.Job, liveHolder, conflictHolder string) (jobs.InsertResult, error) {
	var lastErr error
	for range 3 {
		if j.UniqueKey != "" {
			var existing string
			err := tx.QueryRowContext(ctx, liveHolder, j.Kind, j.HandlerID, j.UniqueKey).Scan(&existing)
			if err == nil {
				return jobs.InsertResult{ID: existing, Kind: j.Kind, HandlerID: j.HandlerID, Duplicate: true}, nil
			} else if err != sql.ErrNoRows {
				return jobs.InsertResult{}, err
			}
		}
		id := jobs.NewID()
		state := jobs.StateAvailable
		if j.AvailableAt.After(now) {
			state = jobs.StatePending
		}
		// The unique index closes races missed by the initial lookup.
		// MySQL reports uniqueness conflicts as error 1062.
		res, err := tx.ExecContext(ctx, `INSERT INTO jobs
(id,kind,handler_id,payload,queue,priority,state,attempt,max_attempts,available_at,timeout_ms,on_timeout,backoff_spec,unique_key,scheduled_for,error,created_at,updated_at)
VALUES (?,?,?,?,?,?,?,0,?,?,?,?,?,?,?,'',?,?)`,
			id, j.Kind, j.HandlerID, j.Payload, j.Queue, j.Priority, string(state), j.MaxAttempts,
			nanos(j.AvailableAt), j.TimeoutMs, int(j.OnTimeout), j.BackoffSpec, j.UniqueKey,
			nullNanos(j.ScheduledFor, j.ScheduledForSet), nanos(now), nanos(now))
		if err != nil {
			if !isDuplicateErr(err) {
				return jobs.InsertResult{}, err
			}
			var existing string
			e2 := tx.QueryRowContext(ctx, conflictHolder, j.Kind, j.HandlerID, j.UniqueKey).Scan(&existing)
			if e2 == sql.ErrNoRows {
				lastErr = err
				continue // holder vanished; the insert wrote nothing
			}
			if e2 != nil {
				return jobs.InsertResult{}, e2
			}
			return jobs.InsertResult{ID: existing, Kind: j.Kind, HandlerID: j.HandlerID, Duplicate: true}, nil
		}
		if n, err := res.RowsAffected(); err != nil {
			return jobs.InsertResult{}, err
		} else if n == 0 {
			var existing string
			e2 := tx.QueryRowContext(ctx, conflictHolder, j.Kind, j.HandlerID, j.UniqueKey).Scan(&existing)
			if e2 == sql.ErrNoRows {
				lastErr = errJobs("unique-key conflict with no live holder")
				continue // holder vanished; the insert wrote nothing
			}
			if e2 != nil {
				return jobs.InsertResult{}, e2
			}
			return jobs.InsertResult{ID: existing, Kind: j.Kind, HandlerID: j.HandlerID, Duplicate: true}, nil
		}
		return jobs.InsertResult{ID: id, CreatedAt: now, Kind: j.Kind, HandlerID: j.HandlerID, ScheduledFor: j.ScheduledFor}, nil
	}
	return jobs.InsertResult{}, lastErr
}

const jobCols = `id,kind,handler_id,payload,queue,priority,state,attempt,max_attempts,available_at,timeout_ms,on_timeout,backoff_spec,unique_key,scheduled_for,error,locked_by,locked_until,cancel_requested,created_at,updated_at`

func scanJob(sc interface{ Scan(...any) error }) (jobs.JobInfo, jobs.ClaimedJob, error) {
	var (
		info                 jobs.JobInfo
		cj                   jobs.ClaimedJob
		state                string
		availableAt, lockedU int64
		schedFor             sql.NullInt64
		onTimeout            int
		cancelReq            int
		created, updated     int64
		timeoutMs            int64
		backoff              []byte
		payload              []byte
		lockedBy             string
		errStr               string
	)
	if err := sc.Scan(&info.ID, &info.Kind, &info.HandlerID, &payload, &info.Queue, &info.Priority,
		&state, &info.Attempt, &info.MaxAttempts, &availableAt, &timeoutMs, &onTimeout, &backoff,
		&info.UniqueKey, &schedFor, &errStr, &lockedBy, &lockedU, &cancelReq, &created, &updated); err != nil {
		return info, cj, err
	}
	info.State = jobs.State(state)
	info.AvailableAt = fromNanos(availableAt)
	info.Timeout = time.Duration(timeoutMs) * time.Millisecond
	info.Error = errStr
	info.CancelRequested = cancelReq != 0
	info.CreatedAt = fromNanos(created)
	info.UpdatedAt = fromNanos(updated)
	if schedFor.Valid {
		info.ScheduledFor = fromNanos(schedFor.Int64)
		info.ScheduledForSet = true
	}
	info.Payload = payload
	cj = jobs.ClaimedJob{
		Job: jobs.Job{
			Kind: info.Kind, HandlerID: info.HandlerID, Payload: payload, Queue: info.Queue,
			Priority: info.Priority, AvailableAt: info.AvailableAt, MaxAttempts: info.MaxAttempts,
			TimeoutMs: timeoutMs, OnTimeout: jobs.OnTimeout(onTimeout), BackoffSpec: backoff,
			UniqueKey: info.UniqueKey, ScheduledFor: info.ScheduledFor, ScheduledForSet: info.ScheduledForSet,
		},
		ID: info.ID, Attempt: info.Attempt, LockedUntil: fromNanos(lockedU),
	}
	return info, cj, nil
}

func (s *Store) Claim(ctx context.Context, req jobs.ClaimRequest) ([]jobs.ClaimedJob, error) {
	if req.WorkerID == "" || len(req.Queues) == 0 || len(req.Handlers) == 0 {
		return nil, nil
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 1
	}
	fetch := limit
	if len(req.QueueLimits) > 0 {
		// Queue limits are applied in Go over a fixed 500-row candidate window.
		fetch = 500
	}

	var args []any
	var b strings.Builder
	b.WriteString("SELECT " + jobCols + " FROM jobs WHERE state IN ('available','pending') AND available_at <= ?")
	args = append(args, nanos(req.Now))
	b.WriteString(" AND queue IN (" + placeholders(len(req.Queues)) + ")")
	for _, q := range req.Queues {
		args = append(args, q)
	}
	b.WriteString(" AND (kind, handler_id) IN (")
	first := true
	for k := range req.Handlers {
		if !first {
			b.WriteString(",")
		}
		first = false
		b.WriteString("(?,?)")
		args = append(args, k.Kind, k.HandlerID)
	}
	b.WriteString(")")
	b.WriteString(" ORDER BY priority DESC, available_at ASC, id ASC LIMIT ?")
	args = append(args, fetch)

	tx, err := s.db.BeginTx(ctx, readCommitted)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // rollback is best-effort cleanup after commit or an earlier error

	// #nosec G202 -- query text contains only fixed SQL and generated placeholders; values remain bound arguments
	rows, err := tx.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	var cands []jobs.ClaimedJob
	for rows.Next() {
		_, cj, err := scanJob(rows)
		if err != nil {
			// #nosec G104 -- read-only row cleanup; the scan error is returned
			rows.Close() //nolint:errcheck // read-only row cleanup; the scan error is returned
			return nil, err
		}
		cands = append(cands, cj)
	}
	// #nosec G104 -- read-only row cleanup; iteration errors are checked via rows.Err
	rows.Close() //nolint:errcheck // read-only row cleanup; iteration errors are checked via rows.Err
	if err := rows.Err(); err != nil {
		return nil, err
	}

	until := req.Now.Add(req.Lease)
	const claimSQL = `UPDATE jobs SET state='running', locked_by=?, locked_until=?, updated_at=?
			 WHERE id=? AND state IN ('available','pending') AND locked_by=''`
	perQueue := map[string]int{}
	var out []jobs.ClaimedJob
	for _, cj := range cands {
		if len(out) >= limit {
			break
		}
		if qlim, ok := req.QueueLimits[cj.Queue]; ok && qlim >= 0 && perQueue[cj.Queue] >= qlim {
			continue
		}
		res, err := tx.ExecContext(ctx, claimSQL, req.WorkerID, nanos(until), nanos(req.Now), cj.ID)
		if err != nil {
			return nil, err
		}
		if n, err := res.RowsAffected(); err != nil {
			return nil, err
		} else if n != 1 {
			// The state transition guarantees that zero rows means a lost race.
			continue
		}
		perQueue[cj.Queue]++
		cj.LockedUntil = until
		out = append(out, cj)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// Heartbeat locks the lease so an unchanged extension reports success.
func (s *Store) Heartbeat(ctx context.Context, jobID, workerID string, now, until time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
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

func (s *Store) Complete(ctx context.Context, jobID, workerID string, now time.Time, o jobs.Outcome) (jobs.State, error) {
	tx, err := s.db.BeginTx(ctx, readCommitted)
	if err != nil {
		return "", err
	}
	defer tx.Rollback() //nolint:errcheck // rollback is best-effort cleanup after commit or an earlier error

	var state, uniqueKey, kind, handler string
	var cancelReq int
	err = tx.QueryRowContext(ctx,
		"SELECT state, cancel_requested, unique_key, kind, handler_id FROM jobs WHERE id=? AND locked_by=?",
		jobID, workerID).Scan(&state, &cancelReq, &uniqueKey, &kind, &handler)
	if err == sql.ErrNoRows || (err == nil && state != string(jobs.StateRunning)) {
		return "", jobs.ErrNotFound
	}
	if err != nil {
		return "", err
	}

	applied := o.State
	attemptState := o.AttemptState
	record := o.AttemptState != ""
	attemptNum := o.Attempt
	if cancelReq != 0 {
		applied = jobs.StateCancelled
		attemptState = jobs.AttemptCancelled
		if !record {
			record = true
		}
	}

	now = now.UTC()
	newAttempt := o.Attempt
	if record {
		if cancelReq != 0 && o.AttemptState == "" {
			var cur int
			if err := tx.QueryRowContext(ctx, "SELECT attempt FROM jobs WHERE id=?", jobID).Scan(&cur); err != nil {
				return "", err
			}
			attemptNum = cur + 1
			newAttempt = attemptNum
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO job_attempts (id,job_id,attempt,worker_id,state,error,started_at,finished_at)
VALUES (?,?,?,?,?,?,?,?)`,
			jobs.NewID(), jobID, attemptNum, workerID, string(attemptState), o.Error, nanos(o.StartedAt), nanos(o.FinishedAt)); err != nil {
			return "", err
		}
	}

	avail := int64(0)
	if applied == jobs.StatePending || applied == jobs.StateAvailable {
		avail = nanos(o.AvailableAt)
	}
	// The ownership guard rolls back the attempt without overwriting newer timestamps.
	res, err := tx.ExecContext(ctx, `UPDATE jobs SET state=?, attempt=?, error=?, available_at=CASE WHEN ?>0 THEN ? ELSE available_at END,
locked_by='', locked_until=0, cancel_requested=0, updated_at=GREATEST(updated_at, ?) WHERE id=? AND locked_by=? AND state='running'`,
		string(applied), newAttempt, o.Error, avail, avail, nanos(now), jobID, workerID)
	if err != nil {
		return "", err
	}
	if n, err := res.RowsAffected(); err != nil {
		return "", err
	} else if n != 1 {
		return "", jobs.ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return applied, nil
}

func (s *Store) SweepExpired(ctx context.Context, now time.Time) (int, error) {
	tx, err := s.db.BeginTx(ctx, readCommitted)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // rollback is best-effort cleanup after commit or an earlier error

	rows, err := tx.QueryContext(ctx,
		"SELECT id, attempt, max_attempts, locked_by, locked_until, cancel_requested FROM jobs WHERE state='running' AND locked_until>0 AND locked_until<=?",
		nanos(now))
	if err != nil {
		return 0, err
	}
	type expired struct {
		id              string
		attempt, maxAtt int
		lockedBy        string
		lockedUntil     int64
		cancelRequested bool
	}
	var list []expired
	for rows.Next() {
		var x expired
		if err := rows.Scan(&x.id, &x.attempt, &x.maxAtt, &x.lockedBy, &x.lockedUntil, &x.cancelRequested); err != nil {
			// #nosec G104 -- read-only row cleanup; the scan error is returned
			rows.Close() //nolint:errcheck // read-only row cleanup; the scan error is returned
			return 0, err
		}
		list = append(list, x)
	}
	// #nosec G104 -- read-only row cleanup; iteration errors are checked via rows.Err
	rows.Close() //nolint:errcheck // read-only row cleanup; iteration errors are checked via rows.Err
	if err := rows.Err(); err != nil {
		return 0, err
	}

	count := 0
	const reclaimSQL = `UPDATE jobs SET state=?, attempt=?, error=?, available_at=?, locked_by='', locked_until=0, cancel_requested=0, updated_at=?
			 WHERE id=? AND state='running' AND locked_by=? AND locked_until=?`
	const attemptSQL = `INSERT INTO job_attempts (id,job_id,attempt,worker_id,state,error,started_at,finished_at)
VALUES (?,?,?,?,?,?,?,?)`
	for _, x := range list {
		attemptNum := x.attempt + 1
		newState := jobs.StateAvailable
		errMsg := ""
		attemptState, attemptErr := jobs.AttemptFailed, "lease expired"
		switch {
		case x.cancelRequested:
			// Cancellation remains terminal across lease recovery.
			newState, errMsg = jobs.StateCancelled, "cancelled"
			attemptState, attemptErr = jobs.AttemptCancelled, "cancelled after lease expiry"
		case attemptNum >= x.maxAtt:
			newState, errMsg = jobs.StateDiscarded, "lease expired"
		}
		// Match the observed lease so a renewal cannot be reclaimed.
		res, err := tx.ExecContext(ctx, reclaimSQL,
			string(newState), attemptNum, errMsg, nanos(now), nanos(now), x.id, x.lockedBy, x.lockedUntil)
		if err != nil {
			return 0, err
		}
		if n, err := res.RowsAffected(); err != nil {
			return 0, err
		} else if n != 1 {
			continue
		}
		if _, err := tx.ExecContext(ctx, attemptSQL,
			jobs.NewID(), x.id, attemptNum, x.lockedBy, string(attemptState), attemptErr, x.lockedUntil, nanos(now)); err != nil {
			return 0, err
		}
		count++
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Store) Get(ctx context.Context, id string) (*jobs.JobInfo, error) {
	// #nosec G202 -- jobCols is a fixed internal column list, not user data
	info, _, err := scanJob(s.db.QueryRowContext(ctx, "SELECT "+jobCols+" FROM jobs WHERE id=?", id))
	if err == sql.ErrNoRows {
		return nil, jobs.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &info, nil
}

func (s *Store) List(ctx context.Context, f jobs.ListFilter) ([]jobs.JobInfo, string, error) {
	cursorTime, cursorID, err := jobs.DecodeCursor(f.Cursor)
	if err != nil {
		return nil, "", err
	}
	limit := jobs.NormalizeLimit(f.Limit)

	var args []any
	var b strings.Builder
	b.WriteString("SELECT " + jobCols + " FROM jobs WHERE 1=1")
	if len(f.Queues) > 0 {
		b.WriteString(" AND queue IN (" + placeholders(len(f.Queues)) + ")")
		for _, q := range f.Queues {
			args = append(args, q)
		}
	}
	if len(f.Kinds) > 0 {
		b.WriteString(" AND kind IN (" + placeholders(len(f.Kinds)) + ")")
		for _, k := range f.Kinds {
			args = append(args, k)
		}
	}
	if len(f.States) > 0 {
		b.WriteString(" AND state IN (" + placeholders(len(f.States)) + ")")
		for _, st := range f.States {
			args = append(args, string(st))
		}
	}
	if f.Cursor != "" {
		b.WriteString(" AND (created_at > ? OR (created_at = ? AND id > ?))")
		args = append(args, nanos(cursorTime), nanos(cursorTime), cursorID)
	}
	b.WriteString(" ORDER BY created_at ASC, id ASC LIMIT ?")
	args = append(args, limit+1)

	// #nosec G202 -- query text contains only fixed SQL and generated placeholders; values remain bound arguments
	rows, err := s.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close() //nolint:errcheck // read-only row cleanup; query errors are returned separately
	var out []jobs.JobInfo
	for rows.Next() {
		info, _, err := scanJob(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, info)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	var next string
	if len(out) > limit {
		last := out[limit-1]
		next = jobs.EncodeCursor(last.CreatedAt, last.ID)
		out = out[:limit]
	}
	return out, next, nil
}

func (s *Store) ListAttempts(ctx context.Context, jobID string, afterAttempt, limit int) ([]jobs.Attempt, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT id,job_id,attempt,worker_id,state,error,started_at,finished_at FROM job_attempts WHERE job_id=? AND attempt>? ORDER BY attempt ASC LIMIT ?",
		jobID, afterAttempt, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read-only row cleanup; query errors are returned separately
	var out []jobs.Attempt
	for rows.Next() {
		var a jobs.Attempt
		var state string
		var started, finished int64
		if err := rows.Scan(&a.ID, &a.JobID, &a.Attempt, &a.WorkerID, &state, &a.Error, &started, &finished); err != nil {
			return nil, err
		}
		a.State = jobs.AttemptState(state)
		a.StartedAt = fromNanos(started)
		a.FinishedAt = fromNanos(finished)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) Retry(ctx context.Context, jobID string, now time.Time) error {
	// Retry only when the conflicting holder vanishes, which guarantees no write occurred.
	var err error
	var conflictNoHolder bool
	for range 3 {
		conflictNoHolder, err = s.retryOnce(ctx, jobID, now)
		if !conflictNoHolder {
			return err
		}
	}
	return err
}

// retryOnce reports whether a uniqueness conflict no longer has a live holder.
func (s *Store) retryOnce(ctx context.Context, jobID string, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, readCommitted)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck // rollback is best-effort cleanup after commit or an earlier error

	const liveOther = "SELECT id FROM jobs WHERE kind=? AND handler_id=? AND unique_key=? AND id<>? AND state NOT IN (" + terminalStates + ") LIMIT 1"
	var state, uniqueKey, kind, handler string
	var attempt, maxAtt int
	err = tx.QueryRowContext(ctx,
		"SELECT state, unique_key, kind, handler_id, attempt, max_attempts FROM jobs WHERE id=?",
		jobID).Scan(&state, &uniqueKey, &kind, &handler, &attempt, &maxAtt)
	if err == sql.ErrNoRows {
		return false, jobs.ErrNotFound
	}
	if err != nil {
		return false, err
	}
	st := jobs.State(state)
	if !st.Terminal() || st == jobs.StateSucceeded {
		return false, jobs.ErrJobNotRetryable
	}
	if uniqueKey != "" {
		var holder string
		err := tx.QueryRowContext(ctx, liveOther, kind, handler, uniqueKey, jobID).Scan(&holder)
		if err == nil {
			return false, &jobs.DuplicateError{ExistingID: holder, Kind: kind, HandlerID: handler, UniqueKey: uniqueKey}
		} else if err != sql.ErrNoRows {
			return false, err
		}
	}
	newMax := maxAtt
	if attempt >= maxAtt {
		newMax = attempt + 1
	}
	res, err := tx.ExecContext(ctx,
		"UPDATE jobs SET state='available', max_attempts=?, available_at=?, error='', locked_by='', locked_until=0, cancel_requested=0, updated_at=? WHERE id=? AND state IN ('failed','discarded','cancelled')",
		newMax, nanos(now), nanos(now), jobID)
	if err != nil {
		if uniqueKey != "" {
			// Classify outside an aborted or stale transaction snapshot.
			// #nosec G104 -- the transaction is already dead; the duplicate lookup replaces it
			tx.Rollback() //nolint:errcheck
			var holder string
			if e := s.db.QueryRowContext(ctx, liveOther, kind, handler, uniqueKey, jobID).Scan(&holder); e == nil {
				return false, &jobs.DuplicateError{ExistingID: holder, Kind: kind, HandlerID: handler, UniqueKey: uniqueKey}
			} else if e == sql.ErrNoRows {
				return true, err
			}
		}
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return false, err
	} else if n != 1 {
		return false, jobs.ErrJobNotRetryable
	}
	return false, tx.Commit()
}

// Cancel locks the job so repeated cancellation remains successful.
func (s *Store) Cancel(ctx context.Context, jobID string, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
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

func (s *Store) Delete(ctx context.Context, jobID string) error {
	tx, err := s.db.BeginTx(ctx, readCommitted)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback is best-effort cleanup after commit or an earlier error
	res, err := tx.ExecContext(ctx, "DELETE FROM jobs WHERE id=? AND state<>'running'", jobID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		var state string
		if e := tx.QueryRowContext(ctx, "SELECT state FROM jobs WHERE id=?", jobID).Scan(&state); e == sql.ErrNoRows {
			return jobs.ErrNotFound
		} else if e != nil {
			return e
		}
		return jobs.ErrJobRunning
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM job_attempts WHERE job_id=?", jobID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) UpsertWorker(ctx context.Context, w jobs.WorkerRow) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO job_workers (id,hostname,queues,started_at,last_seen_at)
VALUES (?,?,?,?,?)
ON DUPLICATE KEY UPDATE hostname=VALUES(hostname), queues=VALUES(queues), last_seen_at=VALUES(last_seen_at)`,
		w.ID, w.Hostname, strings.Join(w.Queues, ","), nanos(w.StartedAt), nanos(w.LastSeenAt))
	return err
}

func (s *Store) RetireWorker(ctx context.Context, workerID string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM job_workers WHERE id=?", workerID)
	return err
}

func (s *Store) ListWorkers(ctx context.Context) ([]jobs.WorkerRow, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id,hostname,queues,started_at,last_seen_at FROM job_workers ORDER BY started_at ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read-only row cleanup; query errors are returned separately
	var out []jobs.WorkerRow
	for rows.Next() {
		var w jobs.WorkerRow
		var queues string
		var started, seen int64
		if err := rows.Scan(&w.ID, &w.Hostname, &queues, &started, &seen); err != nil {
			return nil, err
		}
		if queues != "" {
			w.Queues = strings.Split(queues, ",")
		}
		w.StartedAt = fromNanos(started)
		w.LastSeenAt = fromNanos(seen)
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Store) SweepStaleWorkers(ctx context.Context, olderThan time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM job_workers WHERE last_seen_at>0 AND last_seen_at<?", nanos(olderThan))
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

func (s *Store) ListQueues(ctx context.Context) ([]jobs.QueueInfo, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT queue, state, COUNT(*) FROM jobs GROUP BY queue, state ORDER BY queue")
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read-only row cleanup; query errors are returned separately
	byName := map[string]map[jobs.State]int{}
	var order []string
	for rows.Next() {
		var queue, state string
		var n int
		if err := rows.Scan(&queue, &state, &n); err != nil {
			return nil, err
		}
		if _, ok := byName[queue]; !ok {
			byName[queue] = map[jobs.State]int{}
			order = append(order, queue)
		}
		byName[queue][jobs.State(state)] = n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]jobs.QueueInfo, 0, len(order))
	for _, name := range order {
		out = append(out, jobs.QueueInfo{Name: name, Counts: byName[name]})
	}
	return out, nil
}

func (s *Store) UpsertSchedule(ctx context.Context, row jobs.ScheduleRow) error {
	tx, err := s.db.BeginTx(ctx, readCommitted)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback is best-effort cleanup after commit or an earlier error

	var prevSpec string
	var prevNext int64
	var prevLast sql.NullInt64
	err = tx.QueryRowContext(ctx, "SELECT spec, next_run_at, last_run_at FROM job_schedules WHERE sched_group=? AND name=?",
		row.Group, row.Name).Scan(&prevSpec, &prevNext, &prevLast)
	if err == sql.ErrNoRows {
		_, err = tx.ExecContext(ctx, `INSERT INTO job_schedules (sched_group,name,kind,spec,payload,options_json,next_run_at,last_run_at,updated_at)
VALUES (?,?,?,?,?,?,?,NULL,?)`,
			row.Group, row.Name, row.Kind, row.Spec, row.Payload, row.OptionsJSON, nanos(row.NextRunAt), nanos(row.UpdatedAt))
		if err != nil {
			return err
		}
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	// Retain cadence and last_run when the specification is unchanged.
	next := prevNext
	if prevSpec != row.Spec {
		next = nanos(row.NextRunAt)
	}
	_, err = tx.ExecContext(ctx, `UPDATE job_schedules SET kind=?, spec=?, payload=?, options_json=?, next_run_at=?, updated_at=?
WHERE sched_group=? AND name=?`,
		row.Kind, row.Spec, row.Payload, row.OptionsJSON, next, nanos(row.UpdatedAt), row.Group, row.Name)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) DeleteSchedule(ctx context.Context, group, name string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM job_schedules WHERE sched_group=? AND name=?", group, name)
	return err
}

func (s *Store) ListSchedules(ctx context.Context, group string) ([]jobs.ScheduleRow, error) {
	return s.querySchedules(ctx, "SELECT sched_group,name,kind,spec,payload,options_json,next_run_at,last_run_at,updated_at FROM job_schedules WHERE sched_group=? ORDER BY name", group)
}

func (s *Store) DueSchedules(ctx context.Context, group string, now time.Time) ([]jobs.ScheduleRow, error) {
	return s.querySchedules(ctx, "SELECT sched_group,name,kind,spec,payload,options_json,next_run_at,last_run_at,updated_at FROM job_schedules WHERE sched_group=? AND next_run_at<=? ORDER BY name", group, nanos(now))
}

func (s *Store) querySchedules(ctx context.Context, query string, args ...any) ([]jobs.ScheduleRow, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read-only row cleanup; query errors are returned separately
	var out []jobs.ScheduleRow
	for rows.Next() {
		var r jobs.ScheduleRow
		var next, updated int64
		var last sql.NullInt64
		if err := rows.Scan(&r.Group, &r.Name, &r.Kind, &r.Spec, &r.Payload, &r.OptionsJSON, &next, &last, &updated); err != nil {
			return nil, err
		}
		r.NextRunAt = fromNanos(next)
		if last.Valid {
			r.LastRunAt = fromNanos(last.Int64)
			r.LastRunSet = true
		}
		r.UpdatedAt = fromNanos(updated)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) FireSchedule(ctx context.Context, f jobs.ScheduleFire) (bool, []jobs.InsertResult, error) {
	tx, err := s.db.BeginTx(ctx, readCommitted)
	if err != nil {
		return false, nil, err
	}
	defer tx.Rollback() //nolint:errcheck // rollback is best-effort cleanup after commit or an earlier error

	won, err := s.fireCAS(ctx, tx, f)
	if err != nil {
		return false, nil, err
	}
	if !won {
		return false, nil, nil
	}
	results, err := s.insertTx(ctx, tx, f.Now, f.Jobs)
	if err != nil {
		return false, nil, err
	}
	if err := tx.Commit(); err != nil {
		return false, nil, err
	}
	return true, results, nil
}

// fireCAS locks the schedule so an unchanged election still wins.
func (s *Store) fireCAS(ctx context.Context, tx *sql.Tx, f jobs.ScheduleFire) (bool, error) {
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

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

var (
	_ jobs.Store      = (*Store)(nil)
	_ jobs.TxEnqueuer = (*Store)(nil)
)
