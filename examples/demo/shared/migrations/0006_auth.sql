CREATE TABLE IF NOT EXISTS identities (
    id         TEXT    PRIMARY KEY,
    status     TEXT    NOT NULL,
    claims     TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS password_credentials (
    identity_id TEXT    NOT NULL UNIQUE REFERENCES identities(id) ON DELETE CASCADE,
    email       TEXT    NOT NULL UNIQUE,
    hash        TEXT    NOT NULL,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
