// Package sqlite implements a ratelimit.Store backed by SQLite.
//
// Callers register a driver and should set a busy timeout because the
// store does not retry SQLITE_BUSY:
//
//	sql.Open("sqlite", "file:app.db?_pragma=busy_timeout(5000)")
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/gofabrik/fabrik/ratelimit/internal/sqlstore"
)

const schema = `CREATE TABLE IF NOT EXISTS ratelimit (
    key        TEXT    PRIMARY KEY,
    value      INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS ratelimit_expires_at ON ratelimit(expires_at);`

// Schema returns idempotent DDL for the store.
func Schema() string { return schema }

// Options configures New.
type Options struct {
	// AutoCreate runs Schema during New.
	AutoCreate bool
}

// Store keeps rate-limit entries in a SQLite database.
type Store struct {
	eng *sqlstore.Engine
}

// New constructs a Store and optionally applies Schema.
func New(db *sql.DB, opts Options) (*Store, error) {
	eng, err := sqlstore.New(db, sqliteDialect{}, opts.AutoCreate)
	if err != nil {
		return nil, err
	}
	return &Store{eng: eng}, nil
}

// Get implements ratelimit.Store.
func (s *Store) Get(ctx context.Context, key string, now time.Time) (int64, bool, error) {
	return s.eng.Get(ctx, key, now)
}

// SetIfAbsent implements ratelimit.Store.
func (s *Store) SetIfAbsent(ctx context.Context, key string, value int64, now, expiresAt time.Time) (bool, error) {
	return s.eng.SetIfAbsent(ctx, key, value, now, expiresAt)
}

// CompareAndSwap implements ratelimit.Store.
func (s *Store) CompareAndSwap(ctx context.Context, key string, old, newValue int64, now, expiresAt time.Time) (bool, error) {
	return s.eng.CompareAndSwap(ctx, key, old, newValue, now, expiresAt)
}

// Sweep implements ratelimit.Sweeper.
func (s *Store) Sweep(ctx context.Context, now time.Time) (int64, error) {
	return s.eng.Sweep(ctx, now)
}

type sqliteDialect struct{}

func (sqliteDialect) Placeholder(int) string { return "?" }
func (sqliteDialect) KeyColumn() string      { return "key" }
func (sqliteDialect) Schema() string         { return schema }

// SetIfAbsent reports whether an insert or expired-row overwrite occurred.
func (sqliteDialect) SetIfAbsent(ctx context.Context, db *sql.DB, key string, value int64, now, expiresAt time.Time) (bool, error) {
	res, err := db.ExecContext(ctx,
		`INSERT INTO ratelimit (key, value, expires_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, expires_at = excluded.expires_at
		 WHERE ratelimit.expires_at <= ?`,
		key, value, expiresAt.UnixNano(), now.UnixNano())
	if err != nil {
		return false, fmt.Errorf("ratelimit: set absent %q: %w", key, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("ratelimit: set absent %q: %w", key, err)
	}
	return n == 1, nil
}

// CompareAndSwap relies on SQLite counting matched, unchanged rows.
func (sqliteDialect) CompareAndSwap(ctx context.Context, db *sql.DB, key string, old, newValue int64, now, expiresAt time.Time) (bool, error) {
	res, err := db.ExecContext(ctx,
		`UPDATE ratelimit SET value = ?, expires_at = ?
		 WHERE key = ? AND value = ? AND expires_at > ?`,
		newValue, expiresAt.UnixNano(), key, old, now.UnixNano())
	if err != nil {
		return false, fmt.Errorf("ratelimit: swap %q: %w", key, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("ratelimit: swap %q: %w", key, err)
	}
	return n == 1, nil
}
