// Package sqlite implements a session.Store backed by SQLite.
//
// Callers register a driver and should set a busy timeout because the
// store does not retry SQLITE_BUSY:
//
//	sql.Open("sqlite", "file:app.db?_pragma=busy_timeout(5000)")
package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/gofabrik/fabrik/session"
	"github.com/gofabrik/fabrik/session/internal/sqlstore"
)

const schema = `CREATE TABLE IF NOT EXISTS sessions (
    sid             TEXT    PRIMARY KEY,
    version         INTEGER NOT NULL,
    user_id         TEXT    NOT NULL DEFAULT '',
    absolute_expiry INTEGER NOT NULL,
    idle_expiry     INTEGER NOT NULL,
    payload         BLOB    NOT NULL
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

// Store keeps session records in a SQLite database.
type Store struct {
	eng *sqlstore.Engine
}

// New constructs a Store and optionally applies Schema.
func New(db *sql.DB, opts Options) (*Store, error) {
	eng, err := sqlstore.New(db, sqliteDialect{}, opts.AutoCreate, opts.Now)
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

type sqliteDialect struct{}

func (sqliteDialect) Placeholder(int) string { return "?" }
func (sqliteDialect) Schema() string         { return schema }

// Insert reports whether SQLite inserted a fresh SID.
func (sqliteDialect) Insert(ctx context.Context, db *sql.DB, rec session.Record) (bool, error) {
	res, err := db.ExecContext(ctx, `
		INSERT INTO sessions (sid, version, user_id, absolute_expiry, idle_expiry, payload)
		VALUES (?, 1, ?, ?, ?, ?)
		ON CONFLICT(sid) DO NOTHING`,
		rec.SID, rec.UserID, sqlstore.ToUnixNano(rec.AbsoluteExpiry), sqlstore.ToUnixNano(rec.IdleExpiry), rec.Payload)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// Save relies on the version increment making every matched row change.
func (sqliteDialect) Save(ctx context.Context, db *sql.DB, rec session.Record) (bool, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE sessions
		SET version = version + 1, user_id = ?, absolute_expiry = ?, idle_expiry = ?, payload = ?
		WHERE sid = ? AND version = ?`,
		rec.UserID, sqlstore.ToUnixNano(rec.AbsoluteExpiry), sqlstore.ToUnixNano(rec.IdleExpiry), rec.Payload, rec.SID, rec.Version)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// BumpTTL relies on SQLite counting matched, unchanged rows.
func (sqliteDialect) BumpTTL(ctx context.Context, db *sql.DB, sid string, until, now time.Time) (bool, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE sessions SET idle_expiry = ?
		WHERE sid = ? AND (absolute_expiry = 0 OR absolute_expiry > ?) AND (idle_expiry = 0 OR idle_expiry > ?)`,
		sqlstore.ToUnixNano(until), sid, now.UnixNano(), now.UnixNano())
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
