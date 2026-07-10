package migrations

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"fmt"
	"time"
)

// sqliteDriver runs each migration inside its own BEGIN IMMEDIATE
// transaction on a dedicated connection.
type sqliteDriver struct{}

func (sqliteDriver) placeholder(int) string { return "?" }

func (sqliteDriver) schemaSQL() string {
	return `CREATE TABLE IF NOT EXISTS schema_migrations (
    module     TEXT NOT NULL,
    version    BIGINT NOT NULL,
    name       TEXT NOT NULL,
    checksum   TEXT NOT NULL,
    applied_at TIMESTAMP NOT NULL,
    PRIMARY KEY (module, version)
)`
}

func (sqliteDriver) tableExists(ctx context.Context, q querier) (bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'`)
	if err != nil {
		return false, fmt.Errorf("probe schema_migrations: %w", err)
	}
	defer rows.Close()
	return rows.Next(), rows.Err()
}

func (sqliteDriver) openSession(ctx context.Context, db *sql.DB) (session, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection: %w", err)
	}
	// BEGIN IMMEDIATE needs a connection-local busy timeout under contention.
	var current int64
	if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&current); err != nil {
		conn.Close()
		return nil, fmt.Errorf("read SQLite busy_timeout: %w", err)
	}
	if current <= 0 {
		if _, err := conn.ExecContext(ctx, "PRAGMA busy_timeout = 5000"); err != nil {
			conn.Close()
			return nil, fmt.Errorf("set SQLite busy_timeout: %w", err)
		}
	}
	return &sqliteSession{c: conn}, nil
}

type sqliteSession struct {
	c      *sql.Conn
	closed bool
	// tainted means ROLLBACK failed and the connection cannot reenter the pool.
	tainted bool
}

func (s *sqliteSession) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return s.c.ExecContext(ctx, query, args...)
}

func (s *sqliteSession) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.c.QueryContext(ctx, query, args...)
}

func (s *sqliteSession) apply(ctx context.Context, module string, m migration, insertSQL string) error {
	if _, err := s.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	if _, err := s.ExecContext(ctx, m.body); err != nil {
		s.rollback()
		return err
	}
	if _, err := s.ExecContext(ctx, insertSQL, module, m.version, m.name, m.checksum, time.Now().UTC()); err != nil {
		s.rollback()
		return err
	}
	if _, err := s.ExecContext(ctx, "COMMIT"); err != nil {
		s.rollback()
		return err
	}
	return nil
}

// rollback uses background context so caller cancellation cannot strand the tx.
func (s *sqliteSession) rollback() {
	if _, err := s.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		s.tainted = true
	}
}

func (s *sqliteSession) close() error {
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
