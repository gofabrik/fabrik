// Package postgres implements a session.Store backed by PostgreSQL.
//
// Expiry times preserve the full int64 Unix-nanosecond domain.
package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/gofabrik/fabrik/session"
	"github.com/gofabrik/fabrik/session/internal/sqlstore"
)

const schema = `CREATE TABLE IF NOT EXISTS sessions (
    sid             BYTEA   PRIMARY KEY,
    version         BIGINT  NOT NULL,
    user_id         BYTEA   NOT NULL DEFAULT '',
    absolute_expiry BIGINT  NOT NULL,
    idle_expiry     BIGINT  NOT NULL,
    payload         BYTEA   NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_user_id ON sessions(user_id) WHERE user_id <> '';
CREATE INDEX IF NOT EXISTS sessions_idle_expiry ON sessions(idle_expiry);`

// Schema returns idempotent DDL for the store.
func Schema() string { return schema }

// Options configures New.
type Options struct {
	// AutoCreate runs Schema during New.
	AutoCreate bool

	// Now supplies the wall clock and defaults to time.Now.
	Now func() time.Time
}

// Store keeps session records in a PostgreSQL database.
type Store struct {
	eng *sqlstore.Engine
}

// New constructs a Store and optionally applies Schema.
func New(db *sql.DB, opts Options) (*Store, error) {
	eng, err := sqlstore.New(db, postgresDialect{}, opts.AutoCreate, opts.Now)
	if err != nil {
		return nil, err
	}
	return &Store{eng: eng}, nil
}

// Load implements session.Store.
func (s *Store) Load(ctx context.Context, sid string) (session.Record, error) {
	return s.eng.Load(ctx, sid)
}

// Save implements session.Store.
func (s *Store) Save(ctx context.Context, rec session.Record) (session.Record, error) {
	return s.eng.Save(ctx, rec)
}

// Delete implements session.Store.
func (s *Store) Delete(ctx context.Context, sid string) error {
	return s.eng.Delete(ctx, sid)
}

// BumpTTL implements session.TTLBumper.
func (s *Store) BumpTTL(ctx context.Context, sid string, until time.Time) error {
	return s.eng.BumpTTL(ctx, sid, until)
}

// ListByUser implements session.UserIndexer.
func (s *Store) ListByUser(ctx context.Context, userID string) ([]string, error) {
	return s.eng.ListByUser(ctx, userID)
}

// RevokeByUser implements session.UserIndexer.
func (s *Store) RevokeByUser(ctx context.Context, userID string, except ...string) (int, error) {
	return s.eng.RevokeByUser(ctx, userID, except...)
}

// Scan implements session.Scanner.
func (s *Store) Scan(ctx context.Context, fn func(sid string) bool) error {
	return s.eng.Scan(ctx, fn)
}

// Sweep implements session.Sweeper.
func (s *Store) Sweep(ctx context.Context) (int, error) {
	return s.eng.Sweep(ctx)
}

type postgresDialect struct{}

func (postgresDialect) Placeholder(i int) string { return fmt.Sprintf("$%d", i) }
func (postgresDialect) Schema() string           { return schema }

// ByteArg preserves arbitrary identifiers in BYTEA columns.
func (postgresDialect) ByteArg(v string) any { return []byte(v) }

// Insert reports whether PostgreSQL inserted a fresh SID.
func (postgresDialect) Insert(ctx context.Context, db *sql.DB, rec session.Record) (bool, error) {
	res, err := db.ExecContext(ctx, `
		INSERT INTO sessions (sid, version, user_id, absolute_expiry, idle_expiry, payload)
		VALUES ($1, 1, $2, $3, $4, $5)
		ON CONFLICT(sid) DO NOTHING`,
		[]byte(rec.SID), []byte(rec.UserID), sqlstore.ToUnixNano(rec.AbsoluteExpiry), sqlstore.ToUnixNano(rec.IdleExpiry), rec.Payload)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// Save relies on PostgreSQL counting matched rows.
func (postgresDialect) Save(ctx context.Context, db *sql.DB, rec session.Record) (bool, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE sessions
		SET version = version + 1, user_id = $1, absolute_expiry = $2, idle_expiry = $3, payload = $4
		WHERE sid = $5 AND version = $6`,
		[]byte(rec.UserID), sqlstore.ToUnixNano(rec.AbsoluteExpiry), sqlstore.ToUnixNano(rec.IdleExpiry), rec.Payload, []byte(rec.SID), rec.Version)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// BumpTTL relies on PostgreSQL counting matched, unchanged rows.
func (postgresDialect) BumpTTL(ctx context.Context, db *sql.DB, sid string, until, now time.Time) (bool, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE sessions SET idle_expiry = $1
		WHERE sid = $2 AND (absolute_expiry = 0 OR absolute_expiry > $3) AND (idle_expiry = 0 OR idle_expiry > $4)`,
		sqlstore.ToUnixNano(until), []byte(sid), now.UnixNano(), now.UnixNano())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

var (
	_ session.Store       = (*Store)(nil)
	_ session.TTLBumper   = (*Store)(nil)
	_ session.UserIndexer = (*Store)(nil)
	_ session.Scanner     = (*Store)(nil)
	_ session.Sweeper     = (*Store)(nil)
)
