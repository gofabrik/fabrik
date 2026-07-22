package ratelimit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const sqliteSchema = `CREATE TABLE IF NOT EXISTS ratelimit (
    key        TEXT    PRIMARY KEY,
    value      INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS ratelimit_expires_at ON ratelimit(expires_at);`

// SQLiteSchema returns idempotent DDL for a [SQLiteStore]; production
// deployments should apply it through migrations.
func SQLiteSchema() string {
	return sqliteSchema
}

// SQLiteOptions configures a [SQLiteStore].
type SQLiteOptions struct {
	// AutoCreate runs [SQLiteSchema] during [NewSQLiteStore].
	AutoCreate bool
}

// SQLiteStore is a host-local [Store] backed by database/sql; callers choose
// the driver and should configure busy_timeout because the store does not retry
// SQLITE_BUSY.
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore wraps db with a ratelimit [Store]; AutoCreate applies
// [SQLiteSchema] first.
func NewSQLiteStore(db *sql.DB, opts SQLiteOptions) (*SQLiteStore, error) {
	if db == nil {
		return nil, errors.New("ratelimit.NewSQLiteStore: db is required")
	}
	if opts.AutoCreate {
		if _, err := db.Exec(sqliteSchema); err != nil {
			return nil, fmt.Errorf("ratelimit: create schema: %w", err)
		}
	}
	return &SQLiteStore{db: db}, nil
}

func (s *SQLiteStore) Get(ctx context.Context, key string, now time.Time) (int64, bool, error) {
	var value int64
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM ratelimit WHERE key = ? AND expires_at > ?`,
		key, now.UnixNano()).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("ratelimit: get %q: %w", key, err)
	}
	return value, true, nil
}

func (s *SQLiteStore) SetIfAbsent(ctx context.Context, key string, value int64, now, expiresAt time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO ratelimit (key, value, expires_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, expires_at = excluded.expires_at
		 WHERE ratelimit.expires_at <= ?`,
		key, value, expiresAt.UnixNano(), now.UnixNano())
	if err != nil {
		return false, fmt.Errorf("ratelimit: set %q: %w", key, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("ratelimit: set %q: %w", key, err)
	}
	return n == 1, nil
}

func (s *SQLiteStore) CompareAndSwap(ctx context.Context, key string, old, new int64, now, expiresAt time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE ratelimit SET value = ?, expires_at = ?
		 WHERE key = ? AND value = ? AND expires_at > ?`,
		new, expiresAt.UnixNano(), key, old, now.UnixNano())
	if err != nil {
		return false, fmt.Errorf("ratelimit: swap %q: %w", key, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("ratelimit: swap %q: %w", key, err)
	}
	return n == 1, nil
}

// Sweep deletes every row expired at now and reports how many.
func (s *SQLiteStore) Sweep(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM ratelimit WHERE expires_at <= ?`, now.UnixNano())
	if err != nil {
		return 0, fmt.Errorf("ratelimit: sweep: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("ratelimit: sweep: %w", err)
	}
	return n, nil
}
