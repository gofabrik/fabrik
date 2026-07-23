-- NULL expires_at means no expiry.
CREATE TABLE IF NOT EXISTS cache_entries (
    key        TEXT PRIMARY KEY,
    value      BLOB NOT NULL,
    expires_at INTEGER
);
CREATE INDEX IF NOT EXISTS cache_entries_expires_at ON cache_entries(expires_at);
