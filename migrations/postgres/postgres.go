// Package postgres is the Postgres migration driver. Pass its value to
// the migrations engine:
//
//	migrations.Migrate(ctx, db, postgres.Driver(), src)
package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/gofabrik/fabrik/migrations"
)

// advisoryLockKey is stable across releases.
var advisoryLockKey = func() int64 {
	h := fnv.New64a()
	// #nosec G104 -- hash.Hash.Write never errors
	h.Write([]byte("fabrik/migrations"))
	return int64(h.Sum64())
}()

// Driver returns the stateless Postgres migration driver.
func Driver() migrations.Driver { return driver{} }

// driver serializes migration calls with a dedicated advisory lock.
type driver struct{}

func (driver) Placeholder(i int) string { return fmt.Sprintf("$%d", i) }

func (driver) SchemaSQL() string {
	return `CREATE TABLE IF NOT EXISTS schema_migrations (
    stream     TEXT NOT NULL,
    version    BIGINT NOT NULL,
    name       TEXT NOT NULL,
    checksum   TEXT NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (stream, version)
)`
}

func (driver) TableExists(ctx context.Context, q migrations.Querier) (bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT to_regclass('schema_migrations')`)
	if err != nil {
		return false, fmt.Errorf("probe schema_migrations: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query cleanup; rows.Err reports iteration errors
	if !rows.Next() {
		return false, rows.Err()
	}
	var reg sql.NullString
	if err := rows.Scan(&reg); err != nil {
		return false, err
	}
	return reg.Valid, rows.Err()
}

func (driver) OpenSession(ctx context.Context, db *sql.DB) (migrations.Session, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", advisoryLockKey); err != nil {
		// #nosec G104 -- connection cleanup after the primary advisory-lock error
		conn.Close() //nolint:errcheck // cleanup failure does not replace the advisory-lock error
		return nil, fmt.Errorf("acquire pg advisory lock: %w", err)
	}
	return &session{c: conn}, nil
}

type session struct {
	c      *sql.Conn
	closed bool
}

func (s *session) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return s.c.ExecContext(ctx, query, args...)
}

func (s *session) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.c.QueryContext(ctx, query, args...)
}

func (s *session) Apply(ctx context.Context, stream string, m migrations.Migration, insertSQL string) error {
	tx, err := s.c.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // best-effort cleanup; commit or the operation error determines the result
	// Argument-free Exec uses the simple query protocol.
	if _, err := tx.ExecContext(ctx, m.Body); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, insertSQL, stream, m.Version, m.Name, m.Checksum, time.Now().UTC()); err != nil {
		return err
	}
	return tx.Commit()
}

// Close releases the advisory lock with a fresh context.
func (s *session) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var unlockErr error
	if _, err := s.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", advisoryLockKey); err != nil {
		unlockErr = fmt.Errorf("release pg advisory lock: %w", err)
	}
	_ = s.c.Close()
	return unlockErr
}
