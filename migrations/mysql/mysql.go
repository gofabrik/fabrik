// Package mysql is the MySQL/MariaDB migration driver. Pass its value to
// the migrations engine:
//
//	migrations.Migrate(ctx, db, mysql.Driver(), src)
//
// MySQL commits DDL implicitly, so earlier DDL survives if a later
// statement in the migration fails.
//
// Multi-statement migration bodies require multiStatements=true in the
// DSN; applied_at scanning requires parseTime=true.
package mysql

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/gofabrik/fabrik/migrations"
)

// lockName names the GET_LOCK advisory lock within MySQL's 64-byte limit.
const lockPrefix = "fabrik/migrations/"

// lockTimeout prevents OpenSession from waiting indefinitely.
const lockTimeout = 30 * time.Second

// Driver returns the stateless MySQL/MariaDB migration driver.
func Driver() migrations.Driver { return driver{} }

// driver serializes migration calls with GET_LOCK on a dedicated connection.
type driver struct{}

func (driver) Placeholder(int) string { return "?" }

func (driver) SchemaSQL() string {
	// VARBINARY keeps bounded names byte-exact; DATETIME(6) preserves microseconds.
	return `CREATE TABLE IF NOT EXISTS schema_migrations (
    stream     VARBINARY(191) NOT NULL,
    version    BIGINT NOT NULL,
    name       VARBINARY(191) NOT NULL,
    checksum   VARBINARY(64) NOT NULL,
    applied_at DATETIME(6) NOT NULL,
    PRIMARY KEY (stream, version)
)`
}

func (driver) TableExists(ctx context.Context, q migrations.Querier) (bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'schema_migrations'`)
	if err != nil {
		return false, fmt.Errorf("probe schema_migrations: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query cleanup; rows.Err reports iteration errors
	if !rows.Next() {
		return false, rows.Err()
	}
	var n int
	if err := rows.Scan(&n); err != nil {
		return false, err
	}
	return n > 0, rows.Err()
}

// lockName scopes the server-wide advisory lock to one database.
func lockName(database string) string {
	h := fnv.New64a()
	// #nosec G104 -- hash.Hash.Write never errors
	h.Write([]byte(database))
	return fmt.Sprintf("%s%x", lockPrefix, h.Sum64())
}

func (driver) OpenSession(ctx context.Context, db *sql.DB) (migrations.Session, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection: %w", err)
	}
	var database sql.NullString
	if err := conn.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&database); err != nil {
		// #nosec G104 -- connection cleanup after the database-name error
		conn.Close() //nolint:errcheck // cleanup failure does not replace the database-name error
		return nil, fmt.Errorf("resolve database name: %w", err)
	}
	name := lockName(database.String)
	// GET_LOCK returns 1 on success, 0 on timeout, NULL on error.
	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, ?)", name, int(lockTimeout.Seconds())).Scan(&got); err != nil {
		// #nosec G104 -- connection cleanup after the primary advisory-lock error
		conn.Close() //nolint:errcheck // cleanup failure does not replace the advisory-lock error
		return nil, fmt.Errorf("acquire mysql advisory lock: %w", err)
	}
	if !got.Valid || got.Int64 != 1 {
		// #nosec G104 -- connection cleanup after the lock was not granted
		conn.Close() //nolint:errcheck // cleanup failure does not replace the lock-not-granted error
		return nil, fmt.Errorf("acquire mysql advisory lock %q: not granted within %s", name, lockTimeout)
	}
	return &session{c: conn, lock: name}, nil
}

type session struct {
	c      *sql.Conn
	lock   string
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
	if _, err := tx.ExecContext(ctx, m.Body); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, insertSQL, stream, m.Version, m.Name, m.Checksum, time.Now().UTC()); err != nil {
		return err
	}
	return tx.Commit()
}

// Close releases the advisory lock with a fresh context and evicts the connection unless release is confirmed.
func (s *session) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var unlockErr error
	// RELEASE_LOCK returns 1 on release, 0 when held by another session, NULL
	// when the lock does not exist.
	var released sql.NullInt64
	if err := s.c.QueryRowContext(ctx, "SELECT RELEASE_LOCK(?)", s.lock).Scan(&released); err != nil {
		unlockErr = fmt.Errorf("release mysql advisory lock: %w", err)
	} else if !released.Valid || released.Int64 != 1 {
		unlockErr = fmt.Errorf("release mysql advisory lock %q: not released", s.lock)
	}
	if unlockErr != nil {
		// Mark the driver connection bad so sql.Conn.Close discards it.
		_ = s.c.Raw(func(any) error { return sqldriver.ErrBadConn })
	}
	if cerr := s.c.Close(); cerr != nil && unlockErr == nil {
		return cerr
	}
	return unlockErr
}
