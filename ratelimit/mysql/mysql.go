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

	"github.com/gofabrik/fabrik/ratelimit/internal/sqlstore"
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
	eng *sqlstore.Engine
}

// New constructs a Store and optionally applies Schema.
func New(db *sql.DB, opts Options) (*Store, error) {
	eng, err := sqlstore.New(db, mysqlDialect{}, opts.AutoCreate)
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

type mysqlDialect struct{}

func (mysqlDialect) Placeholder(int) string { return "?" }
func (mysqlDialect) KeyColumn() string      { return "`key`" }
func (mysqlDialect) Schema() string         { return schema }

// SetIfAbsent inserts directly, then locks an existing row before checking
// whether it has expired.
func (mysqlDialect) SetIfAbsent(ctx context.Context, db *sql.DB, key string, value int64, now, expiresAt time.Time) (bool, error) {
	// Plain INSERT keeps strict-mode errors fatal instead of truncating keys.
	_, err := db.ExecContext(ctx,
		"INSERT INTO ratelimit (`key`, value, expires_at) VALUES (?, ?, ?)",
		key, value, expiresAt.UnixNano())
	if err == nil {
		return true, nil
	}
	if !isDuplicateKey(err) {
		return false, fmt.Errorf("ratelimit: set absent %q: %w", key, err)
	}

	tx, err := db.BeginTx(ctx, nil)
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
func (mysqlDialect) CompareAndSwap(ctx context.Context, db *sql.DB, key string, old, newValue int64, now, expiresAt time.Time) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
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

// isDuplicateKey recognizes error 1062 without importing a SQL driver.
func isDuplicateKey(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Error 1062")
}
