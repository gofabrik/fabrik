// Package postgres implements a cache.Store backed by PostgreSQL.
//
// Expiry times preserve the full int64 Unix-nanosecond domain.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/gofabrik/fabrik/cache"
)

const schema = `CREATE TABLE IF NOT EXISTS cache_entries (
    key        BYTEA PRIMARY KEY,
    value      BYTEA NOT NULL,
    expires_at BIGINT
);
CREATE INDEX IF NOT EXISTS cache_entries_expires_at ON cache_entries(expires_at);`

// Schema returns idempotent DDL for the store.
func Schema() string { return schema }

// Options configures New.
type Options struct {
	// AutoCreate runs Schema during New.
	AutoCreate bool
}

// Store keeps cache entries in a PostgreSQL database.
type Store struct {
	db *sql.DB
}

// New constructs a Store and optionally applies Schema.
func New(db *sql.DB, opts Options) (*Store, error) {
	if db == nil {
		return nil, errors.New("cache: db is required")
	}
	if opts.AutoCreate {
		if _, err := db.Exec(schema); err != nil {
			return nil, fmt.Errorf("cache: create schema: %w", err)
		}
	}
	return &Store{db: db}, nil
}

// Get leaves expired rows for Sweep.
func (s *Store) Get(ctx context.Context, key string, now time.Time) (cache.Entry, bool, error) {
	if err := opNow(ctx, "get", key, now); err != nil {
		return cache.Entry{}, false, err
	}
	var value []byte
	var expires sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT value, expires_at FROM cache_entries WHERE key = $1`,
		byteKey(key)).Scan(&value, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return cache.Entry{}, false, nil
	}
	if err != nil {
		return cache.Entry{}, false, fmt.Errorf("cache: get %q: %w", key, err)
	}
	out := cache.Entry{Value: value}
	if expires.Valid {
		out.Expires = time.Unix(0, expires.Int64)
	}
	return out, true, nil
}

// Set implements cache.Store.
func (s *Store) Set(ctx context.Context, key string, e cache.Entry) error {
	if err := opExpiry(ctx, "set", key, e.Expires); err != nil {
		return err
	}
	var expires any
	if !e.Expires.IsZero() {
		expires = e.Expires.UnixNano()
	}
	value := e.Value
	if value == nil {
		// Preserve nil as an empty value rather than SQL NULL.
		value = []byte{}
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO cache_entries (key, value, expires_at) VALUES ($1, $2, $3)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, expires_at = excluded.expires_at`,
		byteKey(key), value, expires); err != nil {
		return fmt.Errorf("cache: set %q: %w", key, err)
	}
	return nil
}

// Delete implements cache.Store.
func (s *Store) Delete(ctx context.Context, key string) error {
	if err := opCtx(ctx, "delete", key); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM cache_entries WHERE key = $1`, byteKey(key)); err != nil {
		return fmt.Errorf("cache: delete %q: %w", key, err)
	}
	return nil
}

// Sweep implements cache.Sweeper.
func (s *Store) Sweep(ctx context.Context, now time.Time) (int, error) {
	if err := opNow(ctx, "sweep", "", now); err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM cache_entries WHERE expires_at IS NOT NULL AND expires_at <= $1`,
		now.UnixNano())
	if err != nil {
		return 0, fmt.Errorf("cache: sweep: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("cache: sweep: %w", err)
	}
	return int(n), nil
}

// byteKey preserves arbitrary key bytes in the BYTEA column.
func byteKey(key string) []byte { return []byte(key) }

var (
	_ cache.Store   = (*Store)(nil)
	_ cache.Sweeper = (*Store)(nil)
)

// minInstant and maxInstant bound the int64 Unix-nanosecond domain.
var (
	minInstant = time.Unix(0, math.MinInt64)
	maxInstant = time.Unix(0, math.MaxInt64)
)

// checkNow returns an error when t is outside the int64 unix-nano domain.
func checkNow(t time.Time) error {
	if t.Before(minInstant) || t.After(maxInstant) {
		return fmt.Errorf("instant %v outside the int64 unix-nano domain", t)
	}
	return nil
}

// checkExpiry accepts zero as the no-expiry sentinel.
func checkExpiry(t time.Time) error {
	if t.IsZero() {
		return nil
	}
	return checkNow(t)
}

// opCtx returns an error when ctx is already canceled.
func opCtx(ctx context.Context, op, key string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("cache: %s %q: %w", op, key, err)
	}
	return nil
}

// opNow validates the context and current time for an operation.
func opNow(ctx context.Context, op, key string, now time.Time) error {
	if err := opCtx(ctx, op, key); err != nil {
		return err
	}
	if now.IsZero() {
		return fmt.Errorf("cache: %s %q: zero instant", op, key)
	}
	if err := checkNow(now); err != nil {
		return fmt.Errorf("cache: %s %q: %w", op, key, err)
	}
	return nil
}

// opExpiry validates the context and expiry for an operation.
func opExpiry(ctx context.Context, op, key string, expires time.Time) error {
	if err := opCtx(ctx, op, key); err != nil {
		return err
	}
	if err := checkExpiry(expires); err != nil {
		return fmt.Errorf("cache: %s %q: %w", op, key, err)
	}
	return nil
}
