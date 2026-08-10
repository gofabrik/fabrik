// Package sqlite is the SQLite migration driver. Pass its value to the
// migrations engine:
//
//	migrations.Migrate(ctx, db, sqlite.Driver(), src)
package sqlite

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"fmt"
	"time"

	"github.com/gofabrik/fabrik/migrations"
)

// Driver returns the stateless SQLite migration driver.
func Driver() migrations.Driver { return driver{} }

// driver runs each migration in a dedicated BEGIN IMMEDIATE transaction.
type driver struct{}

func (driver) Placeholder(int) string { return "?" }

func (driver) SchemaSQL() string {
	return `CREATE TABLE IF NOT EXISTS schema_migrations (
    stream     TEXT NOT NULL,
    version    BIGINT NOT NULL,
    name       TEXT NOT NULL,
    checksum   TEXT NOT NULL,
    applied_at TIMESTAMP NOT NULL,
    PRIMARY KEY (stream, version)
)`
}

func (driver) TableExists(ctx context.Context, q migrations.Querier) (bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'`)
	if err != nil {
		return false, fmt.Errorf("probe schema_migrations: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query cleanup; rows.Err reports iteration errors
	return rows.Next(), rows.Err()
}

func (driver) OpenSession(ctx context.Context, db *sql.DB) (migrations.Session, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection: %w", err)
	}
	// BEGIN IMMEDIATE needs a connection-local busy timeout under contention.
	var current int64
	if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&current); err != nil {
		// #nosec G104 -- connection cleanup after the primary busy-timeout query error
		conn.Close() //nolint:errcheck // cleanup failure does not replace the busy-timeout query error
		return nil, fmt.Errorf("read SQLite busy_timeout: %w", err)
	}
	if current <= 0 {
		if _, err := conn.ExecContext(ctx, "PRAGMA busy_timeout = 5000"); err != nil {
			// #nosec G104 -- connection cleanup after the primary busy-timeout configuration error
			conn.Close() //nolint:errcheck // cleanup failure does not replace the configuration error
			return nil, fmt.Errorf("set SQLite busy_timeout: %w", err)
		}
	}
	return &session{c: conn}, nil
}

type session struct {
	c      *sql.Conn
	closed bool
	// A failed rollback prevents the connection from reentering the pool.
	tainted bool
}

func (s *session) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return s.c.ExecContext(ctx, query, args...)
}

func (s *session) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.c.QueryContext(ctx, query, args...)
}

func (s *session) Apply(ctx context.Context, stream string, m migrations.Migration, insertSQL string) error {
	if _, err := s.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	if _, err := s.ExecContext(ctx, m.Body); err != nil {
		s.rollback()
		return err
	}
	if _, err := s.ExecContext(ctx, insertSQL, stream, m.Version, m.Name, m.Checksum, time.Now().UTC()); err != nil {
		s.rollback()
		return err
	}
	if _, err := s.ExecContext(ctx, "COMMIT"); err != nil {
		s.rollback()
		return err
	}
	return nil
}

// rollback ignores caller cancellation so the transaction cannot be stranded.
func (s *session) rollback() {
	if _, err := s.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		s.tainted = true
	}
}

func (s *session) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	if s.tainted {
		// Force sql.Conn.Close to drop the underlying driver connection.
		_ = s.c.Raw(func(any) error { return sqldriver.ErrBadConn })
	}
	return s.c.Close()
}
