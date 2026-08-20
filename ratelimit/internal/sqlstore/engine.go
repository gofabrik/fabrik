// Package sqlstore implements SQL-backed rate-limit stores.
package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// byteArger binds arbitrary keys to binary database columns.
type byteArger interface {
	ByteArg(s string) any
}

// Dialect supplies backend-specific SQL and atomic writes.
type Dialect interface {
	// Placeholder returns the 1-indexed bound-parameter placeholder.
	Placeholder(i int) string
	// KeyColumn quotes the key column where required by the backend.
	KeyColumn() string
	// Schema returns idempotent DDL.
	Schema() string
	// SetIfAbsent writes an absent or expired entry atomically.
	SetIfAbsent(ctx context.Context, db *sql.DB, key string, value int64, now, expiresAt time.Time) (bool, error)
	// CompareAndSwap reports success for unchanged values that match old.
	CompareAndSwap(ctx context.Context, db *sql.DB, key string, old, newValue int64, now, expiresAt time.Time) (bool, error)
}

// Engine stores rate-limit entries in SQL.
type Engine struct {
	db       *sql.DB
	d        Dialect
	getSQL   string
	sweepSQL string
	keyArg   func(string) any
}

// New constructs an Engine and optionally applies its schema.
func New(db *sql.DB, d Dialect, autoCreate bool) (*Engine, error) {
	if db == nil {
		return nil, errors.New("ratelimit: db is required")
	}
	if d == nil {
		return nil, errors.New("ratelimit: dialect is required")
	}
	if autoCreate {
		if _, err := db.Exec(d.Schema()); err != nil {
			return nil, fmt.Errorf("ratelimit: create schema: %w", err)
		}
	}
	keyArg := func(s string) any { return s }
	if ba, ok := d.(byteArger); ok {
		keyArg = ba.ByteArg
	}
	return &Engine{
		keyArg: keyArg,
		db:     db,
		d:      d,
		getSQL: fmt.Sprintf( // #nosec G202
			`SELECT value FROM ratelimit WHERE %s = %s AND expires_at > %s`,
			d.KeyColumn(), d.Placeholder(1), d.Placeholder(2),
		),
		sweepSQL: fmt.Sprintf( // #nosec G202
			`DELETE FROM ratelimit WHERE expires_at <= %s`,
			d.Placeholder(1),
		),
	}, nil
}

// Get implements ratelimit.Store.
func (e *Engine) Get(ctx context.Context, key string, now time.Time) (int64, bool, error) {
	var value int64
	err := e.db.QueryRowContext(ctx, e.getSQL, e.keyArg(key), now.UnixNano()).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("ratelimit: get %q: %w", key, err)
	}
	return value, true, nil
}

// SetIfAbsent implements ratelimit.Store.
func (e *Engine) SetIfAbsent(ctx context.Context, key string, value int64, now, expiresAt time.Time) (bool, error) {
	return e.d.SetIfAbsent(ctx, e.db, key, value, now, expiresAt)
}

// CompareAndSwap implements ratelimit.Store.
func (e *Engine) CompareAndSwap(ctx context.Context, key string, old, newValue int64, now, expiresAt time.Time) (bool, error) {
	return e.d.CompareAndSwap(ctx, e.db, key, old, newValue, now, expiresAt)
}

// Sweep implements ratelimit.Sweeper.
func (e *Engine) Sweep(ctx context.Context, now time.Time) (int64, error) {
	res, err := e.db.ExecContext(ctx, e.sweepSQL, now.UnixNano())
	if err != nil {
		return 0, fmt.Errorf("ratelimit: sweep: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("ratelimit: sweep: %w", err)
	}
	return n, nil
}
