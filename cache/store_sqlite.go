package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const sqliteSchema = `CREATE TABLE IF NOT EXISTS cache_entries (
    key        TEXT PRIMARY KEY,
    value      BLOB NOT NULL,
    expires_at INTEGER
);
CREATE INDEX IF NOT EXISTS cache_entries_expires_at ON cache_entries(expires_at);`

// SQLiteSchema returns the table definition, safe to apply more
// than once; production
// deployments apply it through migrations. A NULL expires_at means no
// expiry.
func SQLiteSchema() string {
	return sqliteSchema
}

// SQLiteOptions configures NewSQLiteStore.
type SQLiteOptions struct {
	// AutoCreate runs SQLiteSchema during NewSQLiteStore.
	AutoCreate bool
}

// SQLiteStore keeps entries in a SQLite database, surviving restarts
// and shared by every process using that database file;
// callers choose the driver and should configure busy_timeout because
// the store does not retry SQLITE_BUSY, e.g.
//
//	sql.Open("sqlite", "file:app.db?_pragma=busy_timeout(5000)")
//
// Reads never delete: expired rows stay until Sweep removes them.
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore wraps db with a cache Store; AutoCreate applies
// SQLiteSchema first.
func NewSQLiteStore(db *sql.DB, opts SQLiteOptions) (*SQLiteStore, error) {
	if db == nil {
		return nil, errors.New("cache.NewSQLiteStore: db is required")
	}
	if opts.AutoCreate {
		if _, err := db.Exec(sqliteSchema); err != nil {
			return nil, fmt.Errorf("cache: create schema: %w", err)
		}
	}
	return &SQLiteStore{db: db}, nil
}

// Get implements Store.
func (s *SQLiteStore) Get(ctx context.Context, key string, now time.Time) (Entry, bool, error) {
	if err := opNow(ctx, "get", key, now); err != nil {
		return Entry{}, false, err
	}
	var value []byte
	var expires sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT value, expires_at FROM cache_entries WHERE key = ?`, key).Scan(&value, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, fmt.Errorf("cache: get %q: %w", key, err)
	}
	e := Entry{Value: value}
	if expires.Valid {
		e.Expires = time.Unix(0, expires.Int64)
	}
	return e, true, nil
}

// Set implements Store.
func (s *SQLiteStore) Set(ctx context.Context, key string, e Entry) error {
	if err := opExpiry(ctx, "set", key, e.Expires); err != nil {
		return err
	}
	var expires any
	if !e.Expires.IsZero() {
		expires = e.Expires.UnixNano()
	}
	value := e.Value
	if value == nil {
		// nil must become an empty blob because SQL NULL violates the column constraint.
		value = []byte{}
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO cache_entries (key, value, expires_at) VALUES (?, ?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, expires_at = excluded.expires_at`,
		key, value, expires)
	if err != nil {
		return fmt.Errorf("cache: set %q: %w", key, err)
	}
	return nil
}

// Delete implements Store.
func (s *SQLiteStore) Delete(ctx context.Context, key string) error {
	if err := opCtx(ctx, "delete", key); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM cache_entries WHERE key = ?`, key); err != nil {
		return fmt.Errorf("cache: delete %q: %w", key, err)
	}
	return nil
}

// Sweep implements Sweeper.
func (s *SQLiteStore) Sweep(ctx context.Context, now time.Time) (int, error) {
	if err := opNow(ctx, "sweep", "", now); err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM cache_entries WHERE expires_at IS NOT NULL AND expires_at <= ?`, now.UnixNano())
	if err != nil {
		return 0, fmt.Errorf("cache: sweep: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("cache: sweep: %w", err)
	}
	return int(n), nil
}
