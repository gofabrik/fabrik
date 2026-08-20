// Package postgres implements a ratelimit.Store backed by PostgreSQL.
//
// Expiry times preserve the full int64 Unix-nanosecond domain.
package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/gofabrik/fabrik/ratelimit/internal/sqlstore"
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
	eng *sqlstore.Engine
}

// New constructs a Store and optionally applies Schema.
func New(db *sql.DB, opts Options) (*Store, error) {
	eng, err := sqlstore.New(db, postgresDialect{}, opts.AutoCreate)
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

type postgresDialect struct{}

func (postgresDialect) Placeholder(i int) string { return fmt.Sprintf("$%d", i) }
func (postgresDialect) KeyColumn() string        { return "key" }
func (postgresDialect) Schema() string           { return schema }

// ByteArg preserves arbitrary key bytes in the BYTEA column.
func (postgresDialect) ByteArg(s string) any { return []byte(s) }

// SetIfAbsent reports whether an insert or expired-row overwrite occurred.
func (postgresDialect) SetIfAbsent(ctx context.Context, db *sql.DB, key string, value int64, now, expiresAt time.Time) (bool, error) {
	res, err := db.ExecContext(ctx,
		`INSERT INTO ratelimit (key, value, expires_at) VALUES ($1, $2, $3)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, expires_at = excluded.expires_at
		 WHERE ratelimit.expires_at <= $4`,
		[]byte(key), value, expiresAt.UnixNano(), now.UnixNano())
	if err != nil {
		return false, fmt.Errorf("ratelimit: set absent %q: %w", key, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("ratelimit: set absent %q: %w", key, err)
	}
	return n == 1, nil
}

// CompareAndSwap relies on PostgreSQL counting matched, unchanged rows.
func (postgresDialect) CompareAndSwap(ctx context.Context, db *sql.DB, key string, old, newValue int64, now, expiresAt time.Time) (bool, error) {
	res, err := db.ExecContext(ctx,
		`UPDATE ratelimit SET value = $1, expires_at = $2
		 WHERE key = $3 AND value = $4 AND expires_at > $5`,
		newValue, expiresAt.UnixNano(), []byte(key), old, now.UnixNano())
	if err != nil {
		return false, fmt.Errorf("ratelimit: swap %q: %w", key, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("ratelimit: swap %q: %w", key, err)
	}
	return n == 1, nil
}
