CREATE TABLE sessions (
    sid             TEXT    PRIMARY KEY,
    version         INTEGER NOT NULL,
    user_id         TEXT    NOT NULL DEFAULT '',
    absolute_expiry INTEGER NOT NULL,
    idle_expiry     INTEGER NOT NULL,
    payload         BLOB    NOT NULL
);

CREATE INDEX sessions_user_id ON sessions(user_id) WHERE user_id <> '';

CREATE INDEX sessions_idle_expiry ON sessions(idle_expiry);
