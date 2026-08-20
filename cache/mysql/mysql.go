// Package mysql implements a cache.Store backed by MySQL or MariaDB.
//
// Keys compare byte-for-byte and must not exceed the 3072-byte InnoDB
// index limit.
package mysql

import (
	"context"
	"database/sql"
	"time"

	"github.com/gofabrik/fabrik/cache"
	"github.com/gofabrik/fabrik/cache/internal/sqlstore"
)

const schema = "CREATE TABLE IF NOT EXISTS cache_entries (\n" +
	"    `key`      VARBINARY(3072) NOT NULL,\n" +
	"    value      LONGBLOB NOT NULL,\n" +
	"    expires_at BIGINT,\n" +
	"    PRIMARY KEY (`key`),\n" +
	"    INDEX cache_entries_expires_at (expires_at)\n" +
	");"

// Schema returns idempotent DDL for the store.
func Schema() string { return schema }

// Options configures New.
type Options struct {
	// AutoCreate runs Schema during New.
	AutoCreate bool
}

// Store keeps cache entries in a MySQL or MariaDB database.
type Store struct {
	eng *sqlstore.Engine
}

// New constructs a Store and optionally applies Schema.
func New(db *sql.DB, opts Options) (*Store, error) {
	eng, err := sqlstore.New(db, dialect{}, opts.AutoCreate)
	if err != nil {
		return nil, err
	}
	return &Store{eng: eng}, nil
}

// Get implements cache.Store.
func (s *Store) Get(ctx context.Context, key string, now time.Time) (cache.Entry, bool, error) {
	return s.eng.Get(ctx, key, now)
}

// Set implements cache.Store.
func (s *Store) Set(ctx context.Context, key string, e cache.Entry) error {
	return s.eng.Set(ctx, key, e)
}

// Delete implements cache.Store.
func (s *Store) Delete(ctx context.Context, key string) error {
	return s.eng.Delete(ctx, key)
}

// Sweep implements cache.Sweeper.
func (s *Store) Sweep(ctx context.Context, now time.Time) (int, error) {
	return s.eng.Sweep(ctx, now)
}

type dialect struct{}

func (dialect) Placeholder(int) string { return "?" }
func (dialect) KeyColumn() string      { return "`key`" }
func (dialect) Schema() string         { return schema }
func (dialect) UpsertSQL() string {
	return "INSERT INTO cache_entries (`key`, value, expires_at) VALUES (?, ?, ?) " +
		"ON DUPLICATE KEY UPDATE value = VALUES(value), expires_at = VALUES(expires_at)"
}
