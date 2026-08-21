// Package mysql implements a ratelimit.Store backed by MySQL or MariaDB.
//
// Keys compare byte-for-byte and must not exceed the 3072-byte InnoDB
// index limit. Conditional writes lock existing rows because unchanged
// matches and misses both report zero affected rows.
package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofabrik/fabrik/ratelimit"
)

const schema = "CREATE TABLE IF NOT EXISTS ratelimit (\n" +
	"    `key`      VARBINARY(3072) NOT NULL,\n" +
	"    value      BIGINT          NOT NULL,\n" +
	"    expires_at BIGINT          NOT NULL,\n" +
	"    PRIMARY KEY (`key`),\n" +
	"    INDEX ratelimit_expires_at (expires_at)\n" +
	");"

// Schema returns idempotent DDL for the store.
func Schema() string { return schema }

// Options configures New.
type Options struct {
	// AutoCreate runs Schema during New.
	AutoCreate bool
}

// Store keeps rate-limit entries in a MySQL or MariaDB database.
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
		"SELECT value FROM ratelimit WHERE `key` = ? AND expires_at > ?",
		key, now.UnixNano()).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("ratelimit: get %q: %w", key, err)
	}
	return value, true, nil
}

// SetIfAbsent inserts directly, then locks an existing row before checking
// whether it has expired.
func (s *Store) SetIfAbsent(ctx context.Context, key string, value int64, now, expiresAt time.Time) (bool, error) {
	// Plain INSERT keeps strict-mode errors fatal instead of truncating keys.
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO ratelimit (`key`, value, expires_at) VALUES (?, ?, ?)",
		key, value, expiresAt.UnixNano())
	if err == nil {
		return true, nil
	}
	if !isDuplicateKey(err) {
		return false, fmt.Errorf("ratelimit: set absent %q: %w", key, err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("ratelimit: set absent %q: %w", key, err)
	}
	defer tx.Rollback() //nolint:errcheck

	var existingExpiry int64
	err = tx.QueryRowContext(ctx,
		"SELECT expires_at FROM ratelimit WHERE `key` = ? FOR UPDATE", key).Scan(&existingExpiry)
	if errors.Is(err, sql.ErrNoRows) {
		// The conflicting row vanished before it could be locked.
		_, err = tx.ExecContext(ctx,
			"INSERT INTO ratelimit (`key`, value, expires_at) VALUES (?, ?, ?)",
			key, value, expiresAt.UnixNano())
		if err != nil {
			return false, fmt.Errorf("ratelimit: set absent %q: %w", key, err)
		}
		return true, tx.Commit()
	}
	if err != nil {
		return false, fmt.Errorf("ratelimit: set absent %q: %w", key, err)
	}

	if existingExpiry > now.UnixNano() {
		return false, nil // live row; leave it alone
	}

	// The row lock makes this overwrite unconditional.
	_, err = tx.ExecContext(ctx,
		"UPDATE ratelimit SET value = ?, expires_at = ? WHERE `key` = ?",
		value, expiresAt.UnixNano(), key)
	if err != nil {
		return false, fmt.Errorf("ratelimit: set absent %q: %w", key, err)
	}
	return true, tx.Commit()
}

// CompareAndSwap locks the row so an unchanged match still reports success.
func (s *Store) CompareAndSwap(ctx context.Context, key string, old, newValue int64, now, expiresAt time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("ratelimit: swap %q: %w", key, err)
	}
	defer tx.Rollback() //nolint:errcheck

	var currentValue, currentExpiry int64
	err = tx.QueryRowContext(ctx,
		"SELECT value, expires_at FROM ratelimit WHERE `key` = ? FOR UPDATE", key).
		Scan(&currentValue, &currentExpiry)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("ratelimit: swap %q: %w", key, err)
	}

	if currentExpiry <= now.UnixNano() || currentValue != old {
		return false, nil
	}

	_, err = tx.ExecContext(ctx,
		"UPDATE ratelimit SET value = ?, expires_at = ? WHERE `key` = ?",
		newValue, expiresAt.UnixNano(), key)
	if err != nil {
		return false, fmt.Errorf("ratelimit: swap %q: %w", key, err)
	}
	return true, tx.Commit()
}

// Sweep implements ratelimit.Sweeper.
func (s *Store) Sweep(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		"DELETE FROM ratelimit WHERE expires_at <= ?",
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

// isDuplicateKey recognizes error 1062 without importing a SQL driver.
func isDuplicateKey(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Error 1062")
}

var (
	_ ratelimit.Store   = (*Store)(nil)
	_ ratelimit.Sweeper = (*Store)(nil)
)
