-- The jobs schema, applied through migrations (jobs.SQLiteSchema()), so
-- the store is constructed with AutoCreate: false and jobs.Run reconciles
-- schedules against tables these migrations created.
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
);
