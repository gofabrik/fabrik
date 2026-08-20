package postgres_test

import (
	"testing"

	"github.com/gofabrik/fabrik/jobs/postgres"
)

// TestSchemaUnchanged preserves compatibility with deployed schemas.
func TestSchemaUnchanged(t *testing.T) {
	const want = `
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
	if got := postgres.Schema(); got != want {
		t.Fatalf("Schema() diverged from the pinned DDL:\n%s", got)
	}
}

func TestNewRejectsNilDB(t *testing.T) {
	if _, err := postgres.New(nil, postgres.Options{}); err == nil {
		t.Fatal("nil db accepted")
	}
}
