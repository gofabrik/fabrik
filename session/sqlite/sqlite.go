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
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofabrik/fabrik/session"
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
	db  *sql.DB
	now func() time.Time
}

// New constructs a Store and optionally applies Schema.
func New(db *sql.DB, opts Options) (*Store, error) {
	if db == nil {
		return nil, errors.New("session: db is required")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	if opts.AutoCreate {
		if _, err := db.Exec(schema); err != nil {
			return nil, fmt.Errorf("session: create schema: %w", err)
		}
	}
	return &Store{db: db, now: now}, nil
}

// Load implements session.Store.
func (s *Store) Load(ctx context.Context, sid string) (session.Record, error) {
	now := s.now().UnixNano()
	row := s.db.QueryRowContext(ctx,
		`SELECT version, user_id, absolute_expiry, idle_expiry, payload FROM sessions
		 WHERE sid = ? AND (absolute_expiry = 0 OR absolute_expiry > ?) AND (idle_expiry = 0 OR idle_expiry > ?)`,
		sid, now, now)

	var (
		version       uint64
		userID        string
		absExp, idExp int64
		payload       []byte
	)
	if err := row.Scan(&version, &userID, &absExp, &idExp, &payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return session.Record{}, session.ErrNotFound
		}
		return session.Record{}, fmt.Errorf("session: load: %w", err)
	}
	return session.Record{
		SID:            sid,
		Version:        version,
		UserID:         userID,
		AbsoluteExpiry: fromUnixNano(absExp),
		IdleExpiry:     fromUnixNano(idExp),
		Payload:        payload,
	}, nil
}

// Save implements session.Store.
func (s *Store) Save(ctx context.Context, rec session.Record) (session.Record, error) {
	if rec.SID == "" {
		return session.Record{}, errors.New("session: save: empty SID")
	}

	if rec.Version == 0 {
		res, err := s.db.ExecContext(ctx, `
			INSERT INTO sessions (sid, version, user_id, absolute_expiry, idle_expiry, payload)
			VALUES (?, 1, ?, ?, ?, ?)
			ON CONFLICT(sid) DO NOTHING`,
			rec.SID, rec.UserID, toUnixNano(rec.AbsoluteExpiry), toUnixNano(rec.IdleExpiry), rec.Payload)
		if err != nil {
			return session.Record{}, fmt.Errorf("session: insert: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return session.Record{}, fmt.Errorf("session: insert: %w", err)
		}
		if n != 1 {
			return session.Record{}, fmt.Errorf("session: insert collided with existing row: %w", session.ErrVersionConflict)
		}
		stored := rec
		stored.Version = 1
		stored.Payload = clonePayload(rec.Payload)
		return stored, nil
	}

	// The version increment makes every matched row change, so
	// RowsAffected distinguishes a stale CAS from a match.
	res, err := s.db.ExecContext(ctx, `
		UPDATE sessions
		SET version = version + 1, user_id = ?, absolute_expiry = ?, idle_expiry = ?, payload = ?
		WHERE sid = ? AND version = ?`,
		rec.UserID, toUnixNano(rec.AbsoluteExpiry), toUnixNano(rec.IdleExpiry), rec.Payload, rec.SID, rec.Version)
	if err != nil {
		return session.Record{}, fmt.Errorf("session: update: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return session.Record{}, fmt.Errorf("session: update: %w", err)
	}
	if n != 1 {
		return session.Record{}, fmt.Errorf("session: stale write at version %d: %w", rec.Version, session.ErrVersionConflict)
	}
	stored := rec
	stored.Version++
	stored.Payload = clonePayload(rec.Payload)
	return stored, nil
}

// Delete implements session.Store.
func (s *Store) Delete(ctx context.Context, sid string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE sid = ?`, sid); err != nil {
		return fmt.Errorf("session: delete: %w", err)
	}
	return nil
}

// BumpTTL implements session.TTLBumper. SQLite counts matched,
// unchanged rows, so a plain conditional UPDATE suffices.
func (s *Store) BumpTTL(ctx context.Context, sid string, until time.Time) error {
	now := s.now().UnixNano()
	res, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET idle_expiry = ?
		WHERE sid = ? AND (absolute_expiry = 0 OR absolute_expiry > ?) AND (idle_expiry = 0 OR idle_expiry > ?)`,
		toUnixNano(until), sid, now, now)
	if err != nil {
		return fmt.Errorf("session: bump: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("session: bump: %w", err)
	}
	if n != 1 {
		return session.ErrNotFound
	}
	return nil
}

// ListByUser implements session.UserIndexer and omits expired rows.
func (s *Store) ListByUser(ctx context.Context, userID string) ([]string, error) {
	now := s.now().UnixNano()
	rows, err := s.db.QueryContext(ctx,
		`SELECT sid FROM sessions
		 WHERE user_id = ? AND (absolute_expiry = 0 OR absolute_expiry > ?) AND (idle_expiry = 0 OR idle_expiry > ?)`,
		userID, now, now)
	if err != nil {
		return nil, fmt.Errorf("session: list user: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read errors are reported by rows.Err; Close is cleanup
	var sids []string
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return nil, err
		}
		sids = append(sids, sid)
	}
	return sids, rows.Err()
}

// RevokeByUser implements session.UserIndexer. SIDs in except are preserved.
func (s *Store) RevokeByUser(ctx context.Context, userID string, except ...string) (int, error) {
	query := `DELETE FROM sessions WHERE user_id = ?`
	args := []any{userID}
	if len(except) > 0 {
		placeholders := make([]string, len(except))
		for i, sid := range except {
			placeholders[i] = "?"
			args = append(args, sid)
		}
		// #nosec G202 -- builds placeholders, not user data
		query += ` AND sid NOT IN (` + strings.Join(placeholders, ",") + `)`
	}
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("session: revoke user: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// Scan implements session.Scanner.
func (s *Store) Scan(ctx context.Context, fn func(sid string) bool) error {
	now := s.now().UnixNano()
	rows, err := s.db.QueryContext(ctx,
		`SELECT sid FROM sessions WHERE (absolute_expiry = 0 OR absolute_expiry > ?) AND (idle_expiry = 0 OR idle_expiry > ?)`,
		now, now)
	if err != nil {
		return fmt.Errorf("session: scan: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read errors are reported by rows.Err; Close is cleanup
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return err
		}
		if !fn(sid) {
			return nil
		}
	}
	return rows.Err()
}

// Sweep implements session.Sweeper.
func (s *Store) Sweep(ctx context.Context) (int, error) {
	now := s.now().UnixNano()
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE (absolute_expiry > 0 AND absolute_expiry <= ?) OR (idle_expiry > 0 AND idle_expiry <= ?)`,
		now, now)
	if err != nil {
		return 0, fmt.Errorf("session: sweep: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// clonePayload copies payload bytes across the store boundary.
func clonePayload(p []byte) []byte {
	if p == nil {
		return nil
	}
	out := make([]byte, len(p))
	copy(out, p)
	return out
}

// toUnixNano maps zero time to the no-deadline marker.
func toUnixNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func fromUnixNano(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

var (
	_ session.Store       = (*Store)(nil)
	_ session.TTLBumper   = (*Store)(nil)
	_ session.UserIndexer = (*Store)(nil)
	_ session.Scanner     = (*Store)(nil)
	_ session.Sweeper     = (*Store)(nil)
)
