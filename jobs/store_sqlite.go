package jobs

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// sqliteSchema is the idempotent DDL the SQLite store expects. Times are
// unix nanoseconds; nullable columns (scheduled_for, last_run_at) are
// NULL when unset.
const sqliteSchema = `
CREATE TABLE IF NOT EXISTS jobs (
    id               TEXT    PRIMARY KEY,
    kind             TEXT    NOT NULL,
    handler_id       TEXT    NOT NULL,
    payload          BLOB    NOT NULL,
    queue            TEXT    NOT NULL,
    priority         INTEGER NOT NULL DEFAULT 0,
    state            TEXT    NOT NULL,
    attempt          INTEGER NOT NULL DEFAULT 0,
    max_attempts     INTEGER NOT NULL,
    available_at     INTEGER NOT NULL,
    timeout_ms       INTEGER NOT NULL DEFAULT 0,
    on_timeout       INTEGER NOT NULL DEFAULT 0,
    backoff_spec     BLOB,
    unique_key       TEXT    NOT NULL DEFAULT '',
    scheduled_for    INTEGER,
    error            TEXT    NOT NULL DEFAULT '',
    locked_by        TEXT    NOT NULL DEFAULT '',
    locked_until     INTEGER NOT NULL DEFAULT 0,
    cancel_requested INTEGER NOT NULL DEFAULT 0,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
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
    started_at  INTEGER NOT NULL,
    finished_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS job_attempts_job ON job_attempts(job_id, attempt);

CREATE TABLE IF NOT EXISTS job_workers (
    id           TEXT    PRIMARY KEY,
    hostname     TEXT    NOT NULL DEFAULT '',
    queues       TEXT    NOT NULL DEFAULT '',
    started_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS job_schedules (
    sched_group  TEXT    NOT NULL,
    name         TEXT    NOT NULL,
    kind         TEXT    NOT NULL,
    spec         TEXT    NOT NULL,
    payload      BLOB    NOT NULL,
    options_json BLOB    NOT NULL,
    next_run_at  INTEGER NOT NULL,
    last_run_at  INTEGER,
    updated_at   INTEGER NOT NULL,
    PRIMARY KEY (sched_group, name)
);`

// SQLiteSchema returns the idempotent DDL backing a SQLite store.
func SQLiteSchema() string { return sqliteSchema }

// SQLiteOptions configures a [SQLiteStore].
type SQLiteOptions struct {
	// AutoCreate applies [SQLiteSchema] during [NewSQLiteStore].
	AutoCreate bool
}

// SQLiteStore is a durable, single-node [Store] backed by a caller-opened
// *sql.DB. It imports no SQL driver. Guarded UPDATEs prevent double-claim;
// WAL, a busy timeout, and immediate-locked transactions improve
// throughput under contention. For modernc.org/sqlite:
// "file:jobs.db?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_txlock=immediate".
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore constructs a store over db.
func NewSQLiteStore(db *sql.DB, opts SQLiteOptions) (*SQLiteStore, error) {
	if db == nil {
		return nil, fmt.Errorf("jobs.NewSQLiteStore: nil db")
	}
	if opts.AutoCreate {
		if _, err := db.ExecContext(context.Background(), sqliteSchema); err != nil {
			return nil, fmt.Errorf("jobs.NewSQLiteStore: create schema: %w", err)
		}
	}
	return &SQLiteStore{db: db}, nil
}

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

func (s *SQLiteStore) Insert(ctx context.Context, now time.Time, jobs []Job) ([]InsertResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // rollback is best-effort cleanup after commit or an earlier error
	out, err := insertTx(ctx, tx, now, jobs)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// InsertTx is the transactional-enqueue capability ([TxEnqueuer]).
func (s *SQLiteStore) InsertTx(ctx context.Context, tx *sql.Tx, now time.Time, jobs []Job) ([]InsertResult, error) {
	return insertTx(ctx, tx, now, jobs)
}

// insertTx writes a batch inside tx, deduping live unique keys as it goes.
func insertTx(ctx context.Context, tx *sql.Tx, now time.Time, jobs []Job) ([]InsertResult, error) {
	now = now.UTC()
	out := make([]InsertResult, 0, len(jobs))
	for _, j := range jobs {
		if j.UniqueKey != "" {
			var existing string
			err := tx.QueryRowContext(ctx,
				"SELECT id FROM jobs WHERE kind=? AND handler_id=? AND unique_key=? AND state NOT IN ("+terminalStates+") LIMIT 1",
				j.Kind, j.HandlerID, j.UniqueKey).Scan(&existing)
			if err == nil {
				out = append(out, InsertResult{ID: existing, Kind: j.Kind, HandlerID: j.HandlerID, Duplicate: true})
				continue
			} else if err != sql.ErrNoRows {
				return nil, err
			}
		}
		id := NewID()
		state := StateAvailable
		if j.AvailableAt.After(now) {
			state = StatePending
		}
		// The partial UNIQUE index arbitrates races missed by the read.
		res, err := tx.ExecContext(ctx, `INSERT INTO jobs
(id,kind,handler_id,payload,queue,priority,state,attempt,max_attempts,available_at,timeout_ms,on_timeout,backoff_spec,unique_key,scheduled_for,created_at,updated_at)
VALUES (?,?,?,?,?,?,?,0,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`,
			id, j.Kind, j.HandlerID, j.Payload, j.Queue, j.Priority, string(state), j.MaxAttempts,
			nanos(j.AvailableAt), j.TimeoutMs, int(j.OnTimeout), j.BackoffSpec, j.UniqueKey,
			nullNanos(j.ScheduledFor, j.ScheduledForSet), nanos(now), nanos(now))
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			var existing string
			if e := tx.QueryRowContext(ctx,
				"SELECT id FROM jobs WHERE kind=? AND handler_id=? AND unique_key=? AND state NOT IN ("+terminalStates+") LIMIT 1",
				j.Kind, j.HandlerID, j.UniqueKey).Scan(&existing); e != nil {
				return nil, e
			}
			out = append(out, InsertResult{ID: existing, Kind: j.Kind, HandlerID: j.HandlerID, Duplicate: true})
			continue
		}
		out = append(out, InsertResult{ID: id, CreatedAt: now, Kind: j.Kind, HandlerID: j.HandlerID, ScheduledFor: j.ScheduledFor})
	}
	return out, nil
}

const jobCols = `id,kind,handler_id,payload,queue,priority,state,attempt,max_attempts,available_at,timeout_ms,on_timeout,backoff_spec,unique_key,scheduled_for,error,locked_by,locked_until,cancel_requested,created_at,updated_at`

func scanJob(sc interface{ Scan(...any) error }) (JobInfo, ClaimedJob, error) {
	var (
		info                 JobInfo
		cj                   ClaimedJob
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
	info.State = State(state)
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
	cj = ClaimedJob{
		Job: Job{
			Kind: info.Kind, HandlerID: info.HandlerID, Payload: payload, Queue: info.Queue,
			Priority: info.Priority, AvailableAt: info.AvailableAt, MaxAttempts: info.MaxAttempts,
			TimeoutMs: timeoutMs, OnTimeout: OnTimeout(onTimeout), BackoffSpec: backoff,
			UniqueKey: info.UniqueKey, ScheduledFor: info.ScheduledFor, ScheduledForSet: info.ScheduledForSet,
		},
		ID: info.ID, Attempt: info.Attempt, LockedUntil: fromNanos(lockedU),
	}
	return info, cj, nil
}

func (s *SQLiteStore) Claim(ctx context.Context, req ClaimRequest) ([]ClaimedJob, error) {
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
	b.WriteString(" AND (kind, handler_id) IN (VALUES ")
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

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // rollback is best-effort cleanup after commit or an earlier error

	// #nosec G202 -- query text contains only fixed SQL and generated placeholders; values remain bound arguments
	rows, err := tx.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	var cands []ClaimedJob
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
	perQueue := map[string]int{}
	var out []ClaimedJob
	for _, cj := range cands {
		if len(out) >= limit {
			break
		}
		if qlim, ok := req.QueueLimits[cj.Queue]; ok && qlim >= 0 && perQueue[cj.Queue] >= qlim {
			continue
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE jobs SET state='running', locked_by=?, locked_until=?, updated_at=?
			 WHERE id=? AND state IN ('available','pending') AND locked_by=''`,
			req.WorkerID, nanos(until), nanos(req.Now), cj.ID)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			// Lost optimistic race.
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

func (s *SQLiteStore) Heartbeat(ctx context.Context, jobID, workerID string, now, until time.Time) (bool, error) {
	var cancelReq int
	err := s.db.QueryRowContext(ctx,
		"SELECT cancel_requested FROM jobs WHERE id=? AND state='running' AND locked_by=?",
		jobID, workerID).Scan(&cancelReq)
	if err == sql.ErrNoRows {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx,
		"UPDATE jobs SET locked_until=?, updated_at=? WHERE id=? AND locked_by=? AND state='running'",
		nanos(until), nanos(now), jobID, workerID)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		// Lease lost between the select and the extend.
		return false, ErrNotFound
	}
	return cancelReq != 0, nil
}

func (s *SQLiteStore) Complete(ctx context.Context, jobID, workerID string, now time.Time, o Outcome) (State, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback() //nolint:errcheck // rollback is best-effort cleanup after commit or an earlier error

	var state, uniqueKey, kind, handler string
	var cancelReq int
	err = tx.QueryRowContext(ctx,
		"SELECT state, cancel_requested, unique_key, kind, handler_id FROM jobs WHERE id=? AND locked_by=?",
		jobID, workerID).Scan(&state, &cancelReq, &uniqueKey, &kind, &handler)
	if err == sql.ErrNoRows || (err == nil && state != string(StateRunning)) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}

	applied := o.State
	attemptState := o.AttemptState
	record := o.AttemptState != ""
	attemptNum := o.Attempt
	if cancelReq != 0 {
		applied = StateCancelled
		attemptState = AttemptCancelled
		if !record {
			record = true
		}
	}

	now = now.UTC()
	newAttempt := o.Attempt
	if record {
		if cancelReq != 0 && o.AttemptState == "" {
			// Release turned into a cancel.
			var cur int
			if err := tx.QueryRowContext(ctx, "SELECT attempt FROM jobs WHERE id=?", jobID).Scan(&cur); err != nil {
				return "", err
			}
			attemptNum = cur + 1
			newAttempt = attemptNum
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO job_attempts (id,job_id,attempt,worker_id,state,error,started_at,finished_at)
VALUES (?,?,?,?,?,?,?,?)`,
			NewID(), jobID, attemptNum, workerID, string(attemptState), o.Error, nanos(o.StartedAt), nanos(o.FinishedAt)); err != nil {
			return "", err
		}
	}

	avail := int64(0)
	if applied == StatePending || applied == StateAvailable {
		avail = nanos(o.AvailableAt)
	}
	// Ownership guards roll back the attempt insert on a stolen lease.
	// Preserve newer timestamps written by concurrent operations.
	res, err := tx.ExecContext(ctx, `UPDATE jobs SET state=?, attempt=?, error=?, available_at=CASE WHEN ?>0 THEN ? ELSE available_at END,
locked_by='', locked_until=0, cancel_requested=0, updated_at=MAX(updated_at, ?) WHERE id=? AND locked_by=? AND state='running'`,
		string(applied), newAttempt, o.Error, avail, avail, nanos(now), jobID, workerID)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return "", ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return applied, nil
}

func (s *SQLiteStore) SweepExpired(ctx context.Context, now time.Time) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
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
		var e expired
		if err := rows.Scan(&e.id, &e.attempt, &e.maxAtt, &e.lockedBy, &e.lockedUntil, &e.cancelRequested); err != nil {
			// #nosec G104 -- read-only row cleanup; the scan error is returned
			rows.Close() //nolint:errcheck // read-only row cleanup; the scan error is returned
			return 0, err
		}
		list = append(list, e)
	}
	// #nosec G104 -- read-only row cleanup; iteration errors are checked via rows.Err
	rows.Close() //nolint:errcheck // read-only row cleanup; iteration errors are checked via rows.Err
	if err := rows.Err(); err != nil {
		return 0, err
	}

	count := 0
	for _, e := range list {
		attemptNum := e.attempt + 1
		newState := StateAvailable
		errMsg := ""
		attemptState, attemptErr := AttemptFailed, "lease expired"
		switch {
		case e.cancelRequested:
			// Cancellation remains terminal across lease recovery.
			newState, errMsg = StateCancelled, "cancelled"
			attemptState, attemptErr = AttemptCancelled, "cancelled after lease expiry"
		case attemptNum >= e.maxAtt:
			newState, errMsg = StateDiscarded, "lease expired"
		}
		// Reclaim only the lease observed by the select.
		res, err := tx.ExecContext(ctx,
			`UPDATE jobs SET state=?, attempt=?, error=?, available_at=?, locked_by='', locked_until=0, cancel_requested=0, updated_at=?
			 WHERE id=? AND state='running' AND locked_by=? AND locked_until=?`,
			string(newState), attemptNum, errMsg, nanos(now), nanos(now), e.id, e.lockedBy, e.lockedUntil)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO job_attempts (id,job_id,attempt,worker_id,state,error,started_at,finished_at)
VALUES (?,?,?,?,?,?,?,?)`,
			NewID(), e.id, attemptNum, e.lockedBy, string(attemptState), attemptErr, e.lockedUntil, nanos(now)); err != nil {
			return 0, err
		}
		count++
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *SQLiteStore) Get(ctx context.Context, id string) (*JobInfo, error) {
	// #nosec G202 -- jobCols is a fixed internal column list, not user data
	info, _, err := scanJob(s.db.QueryRowContext(ctx, "SELECT "+jobCols+" FROM jobs WHERE id=?", id))
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &info, nil
}

func (s *SQLiteStore) List(ctx context.Context, f ListFilter) ([]JobInfo, string, error) {
	cursorTime, cursorID, err := DecodeCursor(f.Cursor)
	if err != nil {
		return nil, "", err
	}
	limit := NormalizeLimit(f.Limit)

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
	var out []JobInfo
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
		next = EncodeCursor(last.CreatedAt, last.ID)
		out = out[:limit]
	}
	return out, next, nil
}

func (s *SQLiteStore) ListAttempts(ctx context.Context, jobID string, afterAttempt, limit int) ([]Attempt, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT id,job_id,attempt,worker_id,state,error,started_at,finished_at FROM job_attempts WHERE job_id=? AND attempt>? ORDER BY attempt ASC LIMIT ?",
		jobID, afterAttempt, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read-only row cleanup; query errors are returned separately
	var out []Attempt
	for rows.Next() {
		var a Attempt
		var state string
		var started, finished int64
		if err := rows.Scan(&a.ID, &a.JobID, &a.Attempt, &a.WorkerID, &state, &a.Error, &started, &finished); err != nil {
			return nil, err
		}
		a.State = AttemptState(state)
		a.StartedAt = fromNanos(started)
		a.FinishedAt = fromNanos(finished)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) Retry(ctx context.Context, jobID string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback is best-effort cleanup after commit or an earlier error

	var state, uniqueKey, kind, handler string
	var attempt, maxAtt int
	err = tx.QueryRowContext(ctx,
		"SELECT state, unique_key, kind, handler_id, attempt, max_attempts FROM jobs WHERE id=?",
		jobID).Scan(&state, &uniqueKey, &kind, &handler, &attempt, &maxAtt)
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	st := State(state)
	if !st.Terminal() || st == StateSucceeded {
		return ErrJobNotRetryable
	}
	if uniqueKey != "" {
		var holder string
		err := tx.QueryRowContext(ctx,
			"SELECT id FROM jobs WHERE kind=? AND handler_id=? AND unique_key=? AND id<>? AND state NOT IN ("+terminalStates+") LIMIT 1",
			kind, handler, uniqueKey, jobID).Scan(&holder)
		if err == nil {
			return &DuplicateError{ExistingID: holder, Kind: kind, HandlerID: handler, UniqueKey: uniqueKey}
		} else if err != sql.ErrNoRows {
			return err
		}
	}
	newMax := maxAtt
	if attempt >= maxAtt {
		newMax = attempt + 1
	}
	// Retry only the terminal row observed above.
	res, err := tx.ExecContext(ctx,
		"UPDATE jobs SET state='available', max_attempts=?, available_at=?, error='', locked_by='', locked_until=0, cancel_requested=0, updated_at=? WHERE id=? AND state IN ('failed','discarded','cancelled')",
		newMax, nanos(now), nanos(now), jobID)
	if err != nil {
		if uniqueKey != "" {
			var holder string
			if e := tx.QueryRowContext(ctx,
				"SELECT id FROM jobs WHERE kind=? AND handler_id=? AND unique_key=? AND id<>? AND state NOT IN ("+terminalStates+") LIMIT 1",
				kind, handler, uniqueKey, jobID).Scan(&holder); e == nil {
				return &DuplicateError{ExistingID: holder, Kind: kind, HandlerID: handler, UniqueKey: uniqueKey}
			}
		}
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrJobNotRetryable
	}
	return tx.Commit()
}

func (s *SQLiteStore) Cancel(ctx context.Context, jobID string, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck // rollback is best-effort cleanup after commit or an earlier error
	// Guarded writes handle concurrent state changes before the final read.
	res, err := tx.ExecContext(ctx,
		"UPDATE jobs SET state='cancelled', updated_at=? WHERE id=? AND state IN ('pending','available')",
		nanos(now), jobID)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return true, tx.Commit()
	}
	res, err = tx.ExecContext(ctx,
		"UPDATE jobs SET cancel_requested=1, updated_at=? WHERE id=? AND state='running'",
		nanos(now), jobID)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return false, tx.Commit()
	}
	var state string
	err = tx.QueryRowContext(ctx, "SELECT state FROM jobs WHERE id=?", jobID).Scan(&state)
	if err == sql.ErrNoRows {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	return false, ErrJobTerminal
}

func (s *SQLiteStore) Delete(ctx context.Context, jobID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback is best-effort cleanup after commit or an earlier error
	// Delete refuses running rows.
	res, err := tx.ExecContext(ctx, "DELETE FROM jobs WHERE id=? AND state<>'running'", jobID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		var state string
		if e := tx.QueryRowContext(ctx, "SELECT state FROM jobs WHERE id=?", jobID).Scan(&state); e == sql.ErrNoRows {
			return ErrNotFound
		} else if e != nil {
			return e
		}
		return ErrJobRunning
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM job_attempts WHERE job_id=?", jobID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) UpsertWorker(ctx context.Context, w WorkerRow) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO job_workers (id,hostname,queues,started_at,last_seen_at)
VALUES (?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET hostname=excluded.hostname, queues=excluded.queues, last_seen_at=excluded.last_seen_at`,
		w.ID, w.Hostname, strings.Join(w.Queues, ","), nanos(w.StartedAt), nanos(w.LastSeenAt))
	return err
}

func (s *SQLiteStore) RetireWorker(ctx context.Context, workerID string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM job_workers WHERE id=?", workerID)
	return err
}

func (s *SQLiteStore) ListWorkers(ctx context.Context) ([]WorkerRow, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id,hostname,queues,started_at,last_seen_at FROM job_workers ORDER BY started_at ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read-only row cleanup; query errors are returned separately
	var out []WorkerRow
	for rows.Next() {
		var w WorkerRow
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

func (s *SQLiteStore) SweepStaleWorkers(ctx context.Context, olderThan time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM job_workers WHERE last_seen_at>0 AND last_seen_at<?", nanos(olderThan))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *SQLiteStore) ListQueues(ctx context.Context) ([]QueueInfo, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT queue, state, COUNT(*) FROM jobs GROUP BY queue, state ORDER BY queue")
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read-only row cleanup; query errors are returned separately
	byName := map[string]map[State]int{}
	var order []string
	for rows.Next() {
		var queue, state string
		var n int
		if err := rows.Scan(&queue, &state, &n); err != nil {
			return nil, err
		}
		if _, ok := byName[queue]; !ok {
			byName[queue] = map[State]int{}
			order = append(order, queue)
		}
		byName[queue][State(state)] = n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]QueueInfo, 0, len(order))
	for _, name := range order {
		out = append(out, QueueInfo{Name: name, Counts: byName[name]})
	}
	return out, nil
}

func (s *SQLiteStore) UpsertSchedule(ctx context.Context, row ScheduleRow) error {
	tx, err := s.db.BeginTx(ctx, nil)
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
	// Preserve last_run and cadence unless the spec changed.
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

func (s *SQLiteStore) DeleteSchedule(ctx context.Context, group, name string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM job_schedules WHERE sched_group=? AND name=?", group, name)
	return err
}

func (s *SQLiteStore) ListSchedules(ctx context.Context, group string) ([]ScheduleRow, error) {
	return s.querySchedules(ctx, "SELECT sched_group,name,kind,spec,payload,options_json,next_run_at,last_run_at,updated_at FROM job_schedules WHERE sched_group=? ORDER BY name", group)
}

func (s *SQLiteStore) DueSchedules(ctx context.Context, group string, now time.Time) ([]ScheduleRow, error) {
	return s.querySchedules(ctx, "SELECT sched_group,name,kind,spec,payload,options_json,next_run_at,last_run_at,updated_at FROM job_schedules WHERE sched_group=? AND next_run_at<=? ORDER BY name", group, nanos(now))
}

func (s *SQLiteStore) querySchedules(ctx context.Context, query string, args ...any) ([]ScheduleRow, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read-only row cleanup; query errors are returned separately
	var out []ScheduleRow
	for rows.Next() {
		var r ScheduleRow
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

func (s *SQLiteStore) FireSchedule(ctx context.Context, f ScheduleFire) (bool, []InsertResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, nil, err
	}
	defer tx.Rollback() //nolint:errcheck // rollback is best-effort cleanup after commit or an earlier error

	// CAS on last_run_at elects one scheduler per tick.
	var expected any
	if f.ExpectedLastRun.Valid {
		expected = f.ExpectedLastRun.Time.UTC().UnixNano()
	}
	newLast := nullNanos(f.NewLastRun, !f.NewLastRun.IsZero())
	res, err := tx.ExecContext(ctx,
		"UPDATE job_schedules SET last_run_at=?, next_run_at=?, updated_at=? WHERE sched_group=? AND name=? AND last_run_at IS ?",
		newLast, nanos(f.NewNextRun), nanos(f.Now), f.Group, f.Name, expected)
	if err != nil {
		return false, nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		// Missing schedule or lost scheduler race.
		var exists int
		if e := tx.QueryRowContext(ctx, "SELECT 1 FROM job_schedules WHERE sched_group=? AND name=?", f.Group, f.Name).Scan(&exists); e == sql.ErrNoRows {
			return false, nil, ErrNotFound
		} else if e != nil {
			return false, nil, e
		}
		return false, nil, nil
	}
	results, err := insertTx(ctx, tx, f.Now, f.Jobs)
	if err != nil {
		return false, nil, err
	}
	if err := tx.Commit(); err != nil {
		return false, nil, err
	}
	return true, results, nil
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

var _ TxEnqueuer = (*SQLiteStore)(nil)
