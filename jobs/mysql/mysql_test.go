package mysql_test

import (
	"testing"

	"github.com/gofabrik/fabrik/jobs/mysql"
)

// TestSchemaUnchanged preserves compatibility with deployed schemas.
func TestSchemaUnchanged(t *testing.T) {
	const want = `
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
	if got := mysql.Schema(); got != want {
		t.Fatalf("Schema() diverged from the pinned DDL:\n%s", got)
	}
}

func TestNewRejectsNilDB(t *testing.T) {
	if _, err := mysql.New(nil, mysql.Options{}); err == nil {
		t.Fatal("nil db accepted")
	}
}
