package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"
	"time"
)

// pgAdvisoryLockKey is stable across releases.
var pgAdvisoryLockKey = func() int64 {
	h := fnv.New64a()
	h.Write([]byte("fabrik/migrations"))
	return int64(h.Sum64())
}()

// pgDriver holds pg_advisory_lock on a dedicated connection for the
// duration of a Migrate call, serializing concurrent runs.
type pgDriver struct{}

func (pgDriver) placeholder(i int) string { return fmt.Sprintf("$%d", i) }

func (pgDriver) schemaSQL() string {
	return `CREATE TABLE IF NOT EXISTS schema_migrations (
    stream     TEXT NOT NULL,
    version    BIGINT NOT NULL,
    name       TEXT NOT NULL,
    checksum   TEXT NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (stream, version)
)`
}

func (pgDriver) tableExists(ctx context.Context, q querier) (bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT to_regclass('schema_migrations')`)
	if err != nil {
		return false, fmt.Errorf("probe schema_migrations: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return false, rows.Err()
	}
	var reg sql.NullString
	if err := rows.Scan(&reg); err != nil {
		return false, err
	}
	return reg.Valid, rows.Err()
}

func (pgDriver) openSession(ctx context.Context, db *sql.DB) (session, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", pgAdvisoryLockKey); err != nil {
		conn.Close()
		return nil, fmt.Errorf("acquire pg advisory lock: %w", err)
	}
	return &pgSession{c: conn}, nil
}

type pgSession struct {
	c      *sql.Conn
	closed bool
}

func (s *pgSession) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return s.c.ExecContext(ctx, query, args...)
}

func (s *pgSession) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.c.QueryContext(ctx, query, args...)
}

func (s *pgSession) apply(ctx context.Context, stream string, m migration, insertSQL string) error {
	tx, err := s.c.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Argument-free Exec uses the simple query protocol.
	if _, err := tx.ExecContext(ctx, m.body); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, insertSQL, stream, m.version, m.name, m.checksum, time.Now().UTC()); err != nil {
		return err
	}
	return tx.Commit()
}

// close releases the advisory lock with a fresh context.
func (s *pgSession) close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var unlockErr error
	if _, err := s.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", pgAdvisoryLockKey); err != nil {
		unlockErr = fmt.Errorf("release pg advisory lock: %w", err)
	}
	_ = s.c.Close()
	return unlockErr
}
