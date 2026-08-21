// Package mysql implements a session.Store backed by MySQL or MariaDB.
//
// SIDs compare byte-for-byte and must not exceed 3072 bytes. User IDs
// compare byte-for-byte and must not exceed 191 bytes. BumpTTL locks the
// row because unchanged matches and misses both report zero affected rows.
package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofabrik/fabrik/session"
)

const schema = "CREATE TABLE IF NOT EXISTS sessions (\n" +
	"    sid             VARBINARY(3072) NOT NULL,\n" +
	"    version         BIGINT          NOT NULL,\n" +
	"    user_id         VARBINARY(191)  NOT NULL DEFAULT '',\n" +
	"    absolute_expiry BIGINT          NOT NULL,\n" +
	"    idle_expiry     BIGINT          NOT NULL,\n" +
	"    payload         LONGBLOB        NOT NULL,\n" +
	"    PRIMARY KEY (sid),\n" +
	"    INDEX sessions_user_id (user_id),\n" +
	"    INDEX sessions_idle_expiry (idle_expiry)\n" +
	");"

// Schema returns idempotent DDL for the store.
func Schema() string { return schema }

// Options configures New.
type Options struct {
	// AutoCreate runs Schema during New.
	AutoCreate bool

	// Now supplies the wall clock and defaults to time.Now.
	Now func() time.Time
}

// Store keeps session records in a MySQL or MariaDB database.
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
		return session.Record{}, fmt.Errorf("session: load %s: %w", sid, err)
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

// Save implements session.Store. The insert path keeps strict-mode
// errors fatal and reports duplicate SIDs as collisions.
func (s *Store) Save(ctx context.Context, rec session.Record) (session.Record, error) {
	if rec.SID == "" {
		return session.Record{}, errors.New("session: save: empty SID")
	}

	if rec.Version == 0 {
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO sessions (sid, version, user_id, absolute_expiry, idle_expiry, payload)
			VALUES (?, 1, ?, ?, ?, ?)`,
			rec.SID, rec.UserID, toUnixNano(rec.AbsoluteExpiry), toUnixNano(rec.IdleExpiry), rec.Payload)
		if err != nil {
			if isDuplicateKey(err) {
				return session.Record{}, fmt.Errorf("session: insert %s collided with existing row: %w", rec.SID, session.ErrVersionConflict)
			}
			return session.Record{}, fmt.Errorf("session: insert %s: %w", rec.SID, err)
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
		return session.Record{}, fmt.Errorf("session: update %s: %w", rec.SID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return session.Record{}, fmt.Errorf("session: update %s: %w", rec.SID, err)
	}
	if n != 1 {
		return session.Record{}, fmt.Errorf("session: stale write on %s at version %d: %w", rec.SID, rec.Version, session.ErrVersionConflict)
	}
	stored := rec
	stored.Version++
	stored.Payload = clonePayload(rec.Payload)
	return stored, nil
}

// Delete implements session.Store.
func (s *Store) Delete(ctx context.Context, sid string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE sid = ?`, sid); err != nil {
		return fmt.Errorf("session: delete %s: %w", sid, err)
	}
	return nil
}

// BumpTTL implements session.TTLBumper. The row is locked because
// MySQL reports zero affected rows for both unchanged matches and
// misses, which a plain UPDATE cannot tell apart.
func (s *Store) BumpTTL(ctx context.Context, sid string, until time.Time) error {
	// Sample the clock before the transaction so lock waits cannot
	// expire a session that was live when the call began.
	ok, err := s.bumpTTL(ctx, sid, until, s.now())
	if err != nil {
		return fmt.Errorf("session: bump %s: %w", sid, err)
	}
	if !ok {
		return session.ErrNotFound
	}
	return nil
}

func (s *Store) bumpTTL(ctx context.Context, sid string, until time.Time, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck

	var absExp, idleExp int64
	err = tx.QueryRowContext(ctx,
		`SELECT absolute_expiry, idle_expiry FROM sessions WHERE sid = ? FOR UPDATE`, sid).
		Scan(&absExp, &idleExp)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	nowNano := now.UnixNano()
	if (absExp != 0 && absExp <= nowNano) || (idleExp != 0 && idleExp <= nowNano) {
		return false, nil
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET idle_expiry = ? WHERE sid = ?`,
		toUnixNano(until), sid); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ListByUser implements session.UserIndexer and omits expired rows.
func (s *Store) ListByUser(ctx context.Context, userID string) ([]string, error) {
	now := s.now().UnixNano()
	rows, err := s.db.QueryContext(ctx,
		`SELECT sid FROM sessions
		 WHERE user_id = ? AND (absolute_expiry = 0 OR absolute_expiry > ?) AND (idle_expiry = 0 OR idle_expiry > ?)`,
		userID, now, now)
	if err != nil {
		return nil, fmt.Errorf("session: list user %s: %w", userID, err)
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
		return 0, fmt.Errorf("session: revoke user %s: %w", userID, err)
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

// isDuplicateKey recognizes error 1062 without importing a SQL driver.
func isDuplicateKey(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Error 1062")
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
