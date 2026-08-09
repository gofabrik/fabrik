// Package sqlstore implements SQL-backed cache stores.
package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/gofabrik/fabrik/cache"
	"github.com/gofabrik/fabrik/cache/internal/cacheutil"
)

// byteArger binds arbitrary keys to binary database columns.
type byteArger interface {
	ByteArg(s string) any
}

// Dialect supplies backend-specific SQL.
type Dialect interface {
	// Placeholder returns the 1-indexed bound-parameter placeholder.
	Placeholder(i int) string
	// KeyColumn quotes the key column where required by the backend.
	KeyColumn() string
	// Schema returns idempotent DDL.
	Schema() string
	// UpsertSQL binds key, value, and expires_at in that order.
	UpsertSQL() string
}

// Engine stores cache entries in SQL. Reads leave expired rows for Sweep.
type Engine struct {
	db        *sql.DB
	keyArg    func(string) any
	getSQL    string
	upsertSQL string
	deleteSQL string
	sweepSQL  string
}

// New constructs an Engine and optionally applies its schema.
func New(db *sql.DB, d Dialect, autoCreate bool) (*Engine, error) {
	if db == nil {
		return nil, errors.New("cache: db is required")
	}
	if d == nil {
		return nil, errors.New("cache: dialect is required")
	}
	if autoCreate {
		if _, err := db.Exec(d.Schema()); err != nil {
			return nil, fmt.Errorf("cache: create schema: %w", err)
		}
	}
	keyArg := func(s string) any { return s }
	if ba, ok := d.(byteArger); ok {
		keyArg = ba.ByteArg
	}
	return &Engine{
		db:        db,
		keyArg:    keyArg,
		getSQL:    fmt.Sprintf(`SELECT value, expires_at FROM cache_entries WHERE %s = %s`, d.KeyColumn(), d.Placeholder(1)), // #nosec G202
		upsertSQL: d.UpsertSQL(),
		deleteSQL: fmt.Sprintf(`DELETE FROM cache_entries WHERE %s = %s`, d.KeyColumn(), d.Placeholder(1)),                      // #nosec G202
		sweepSQL:  fmt.Sprintf(`DELETE FROM cache_entries WHERE expires_at IS NOT NULL AND expires_at <= %s`, d.Placeholder(1)), // #nosec G202
	}, nil
}

// Get implements cache.Store.
func (e *Engine) Get(ctx context.Context, key string, now time.Time) (cache.Entry, bool, error) {
	if err := cacheutil.OpNow(ctx, "get", key, now); err != nil {
		return cache.Entry{}, false, err
	}
	var value []byte
	var expires sql.NullInt64
	err := e.db.QueryRowContext(ctx, e.getSQL, e.keyArg(key)).Scan(&value, &expires)
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
func (e *Engine) Set(ctx context.Context, key string, entry cache.Entry) error {
	if err := cacheutil.OpExpiry(ctx, "set", key, entry.Expires); err != nil {
		return err
	}
	var expires any
	if !entry.Expires.IsZero() {
		expires = entry.Expires.UnixNano()
	}
	value := entry.Value
	if value == nil {
		// Preserve nil as an empty value rather than SQL NULL.
		value = []byte{}
	}
	if _, err := e.db.ExecContext(ctx, e.upsertSQL, e.keyArg(key), value, expires); err != nil {
		return fmt.Errorf("cache: set %q: %w", key, err)
	}
	return nil
}

// Delete implements cache.Store.
func (e *Engine) Delete(ctx context.Context, key string) error {
	if err := cacheutil.OpCtx(ctx, "delete", key); err != nil {
		return err
	}
	if _, err := e.db.ExecContext(ctx, e.deleteSQL, e.keyArg(key)); err != nil {
		return fmt.Errorf("cache: delete %q: %w", key, err)
	}
	return nil
}

// Sweep implements cache.Sweeper.
func (e *Engine) Sweep(ctx context.Context, now time.Time) (int, error) {
	if err := cacheutil.OpNow(ctx, "sweep", "", now); err != nil {
		return 0, err
	}
	res, err := e.db.ExecContext(ctx, e.sweepSQL, now.UnixNano())
	if err != nil {
		return 0, fmt.Errorf("cache: sweep: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("cache: sweep: %w", err)
	}
	return int(n), nil
}
