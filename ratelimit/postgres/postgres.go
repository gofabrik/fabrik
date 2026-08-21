// Package postgres implements a ratelimit.Store backed by PostgreSQL.
//
// Expiry times preserve the full int64 Unix-nanosecond domain.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/gofabrik/fabrik/ratelimit"
)

const schema = `CREATE TABLE IF NOT EXISTS ratelimit (
    key        BYTEA   PRIMARY KEY,
    value      BIGINT  NOT NULL,
    expires_at BIGINT  NOT NULL
);
CREATE INDEX IF NOT EXISTS ratelimit_expires_at ON ratelimit(expires_at);`

// Schema returns idempotent DDL for the store.
func Schema() string { return schema }

// Options configures New.
type Options struct {
	// AutoCreate runs Schema during New.
	AutoCreate bool
}

// Store keeps rate-limit entries in a PostgreSQL database.
type Store struct {
	db *sql.DB
}

// New constructs a Store and optionally applies Schema.
func New(db *sql.DB, opts Options) (*Store, error) {
	if db == nil {
		return nil, errors.New("ratelimit: db is required")
	}
	if opts.AutoCreate {
		if _, err := db.Exec(schema); err != nil {
			return nil, fmt.Errorf("ratelimit: create schema: %w", err)
		}
	}
	return &Store{db: db}, nil
}

// Get implements ratelimit.Store.
func (s *Store) Get(ctx context.Context, key string, now time.Time) (int64, bool, error) {
	var value int64
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM ratelimit WHERE key = $1 AND expires_at > $2`,
		byteKey(key), now.UnixNano()).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("ratelimit: get %q: %w", key, err)
	}
	return value, true, nil
}

// SetIfAbsent reports whether an insert or expired-row overwrite occurred.
func (s *Store) SetIfAbsent(ctx context.Context, key string, value int64, now, expiresAt time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO ratelimit (key, value, expires_at) VALUES ($1, $2, $3)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, expires_at = excluded.expires_at
		 WHERE ratelimit.expires_at <= $4`,
		byteKey(key), value, expiresAt.UnixNano(), now.UnixNano())
	if err != nil {
		return false, fmt.Errorf("ratelimit: set absent %q: %w", key, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("ratelimit: set absent %q: %w", key, err)
	}
	return n == 1, nil
}

// CompareAndSwap implements ratelimit.Store.
// PostgreSQL counts matched, unchanged rows in RowsAffected.
func (s *Store) CompareAndSwap(ctx context.Context, key string, old, newValue int64, now, expiresAt time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE ratelimit SET value = $1, expires_at = $2
		 WHERE key = $3 AND value = $4 AND expires_at > $5`,
		newValue, expiresAt.UnixNano(), byteKey(key), old, now.UnixNano())
	if err != nil {
		return false, fmt.Errorf("ratelimit: swap %q: %w", key, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("ratelimit: swap %q: %w", key, err)
	}
	return n == 1, nil
}

// Sweep implements ratelimit.Sweeper.
func (s *Store) Sweep(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM ratelimit WHERE expires_at <= $1`,
		now.UnixNano())
	if err != nil {
		return 0, fmt.Errorf("ratelimit: sweep: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("ratelimit: sweep: %w", err)
	}
	return n, nil
}

// byteKey preserves arbitrary key bytes in the BYTEA column.
func byteKey(key string) []byte { return []byte(key) }

var (
	_ ratelimit.Store   = (*Store)(nil)
	_ ratelimit.Sweeper = (*Store)(nil)
)
