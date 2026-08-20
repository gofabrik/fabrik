// Package sqlstore implements SQL-backed session stores.
package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofabrik/fabrik/session"
	"github.com/gofabrik/fabrik/session/internal/sessionutil"
)

// byteArger binds arbitrary identifiers to binary database columns.
type byteArger interface {
	ByteArg(s string) any
}

// Dialect supplies backend-specific SQL and conditional writes.
type Dialect interface {
	// Placeholder returns the 1-indexed bound-parameter placeholder.
	Placeholder(i int) string
	// Schema returns idempotent DDL.
	Schema() string
	// Insert reports false when the SID already exists.
	Insert(ctx context.Context, db *sql.DB, rec session.Record) (ok bool, err error)
	// Save increments the version on a matching CAS update.
	Save(ctx context.Context, db *sql.DB, rec session.Record) (ok bool, err error)
	// BumpTTL reports false for missing or expired sessions.
	BumpTTL(ctx context.Context, db *sql.DB, sid string, until, now time.Time) (ok bool, err error)
}

// Engine stores session records in SQL.
type Engine struct {
	db  *sql.DB
	d   Dialect
	now func() time.Time

	loadSQL   string
	deleteSQL string
	listSQL   string
	scanSQL   string
	sweepSQL  string
	keyArg    func(string) any
}

// New constructs an Engine. A nil now defaults to time.Now.
func New(db *sql.DB, d Dialect, autoCreate bool, now func() time.Time) (*Engine, error) {
	if db == nil {
		return nil, errors.New("session: db is required")
	}
	if d == nil {
		return nil, errors.New("session: dialect is required")
	}
	if now == nil {
		now = time.Now
	}
	if autoCreate {
		if _, err := db.Exec(d.Schema()); err != nil {
			return nil, fmt.Errorf("session: create schema: %w", err)
		}
	}
	keyArg := func(v string) any { return v }
	if ba, ok := d.(byteArger); ok {
		keyArg = ba.ByteArg
	}
	return &Engine{
		keyArg: keyArg,
		db:     db,
		d:      d,
		now:    now,
		loadSQL: fmt.Sprintf( // #nosec G202
			`SELECT version, user_id, absolute_expiry, idle_expiry, payload FROM sessions
			 WHERE sid = %s AND (absolute_expiry = 0 OR absolute_expiry > %s) AND (idle_expiry = 0 OR idle_expiry > %s)`,
			d.Placeholder(1), d.Placeholder(2), d.Placeholder(3),
		),
		deleteSQL: fmt.Sprintf(`DELETE FROM sessions WHERE sid = %s`, d.Placeholder(1)), // #nosec G202
		listSQL: fmt.Sprintf( // #nosec G202
			`SELECT sid FROM sessions
			 WHERE user_id = %s AND (absolute_expiry = 0 OR absolute_expiry > %s) AND (idle_expiry = 0 OR idle_expiry > %s)`,
			d.Placeholder(1), d.Placeholder(2), d.Placeholder(3),
		),
		scanSQL: fmt.Sprintf( // #nosec G202
			`SELECT sid FROM sessions WHERE (absolute_expiry = 0 OR absolute_expiry > %s) AND (idle_expiry = 0 OR idle_expiry > %s)`,
			d.Placeholder(1), d.Placeholder(2),
		),
		sweepSQL: fmt.Sprintf( // #nosec G202
			`DELETE FROM sessions WHERE (absolute_expiry > 0 AND absolute_expiry <= %s) OR (idle_expiry > 0 AND idle_expiry <= %s)`,
			d.Placeholder(1), d.Placeholder(2),
		),
	}, nil
}

// Load implements session.Store.
func (e *Engine) Load(ctx context.Context, sid string) (session.Record, error) {
	now := e.now().UnixNano()
	row := e.db.QueryRowContext(ctx, e.loadSQL, e.keyArg(sid), now, now)

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

// Save implements session.Store.
func (e *Engine) Save(ctx context.Context, rec session.Record) (session.Record, error) {
	if rec.SID == "" {
		return session.Record{}, errors.New("session: save: empty SID")
	}

	if rec.Version == 0 {
		ok, err := e.d.Insert(ctx, e.db, rec)
		if err != nil {
			return session.Record{}, fmt.Errorf("session: insert %s: %w", rec.SID, err)
		}
		if !ok {
			return session.Record{}, fmt.Errorf("session: insert %s collided with existing row: %w", rec.SID, session.ErrVersionConflict)
		}
		stored := rec
		stored.Version = 1
		stored.Payload = sessionutil.ClonePayload(rec.Payload)
		return stored, nil
	}

	ok, err := e.d.Save(ctx, e.db, rec)
	if err != nil {
		return session.Record{}, fmt.Errorf("session: update %s: %w", rec.SID, err)
	}
	if !ok {
		return session.Record{}, fmt.Errorf("session: stale write on %s at version %d: %w", rec.SID, rec.Version, session.ErrVersionConflict)
	}
	stored := rec
	stored.Version++
	stored.Payload = sessionutil.ClonePayload(rec.Payload)
	return stored, nil
}

// Delete implements session.Store.
func (e *Engine) Delete(ctx context.Context, sid string) error {
	if _, err := e.db.ExecContext(ctx, e.deleteSQL, e.keyArg(sid)); err != nil {
		return fmt.Errorf("session: delete %s: %w", sid, err)
	}
	return nil
}

// BumpTTL implements session.TTLBumper.
func (e *Engine) BumpTTL(ctx context.Context, sid string, until time.Time) error {
	ok, err := e.d.BumpTTL(ctx, e.db, sid, until, e.now())
	if err != nil {
		return fmt.Errorf("session: bump %s: %w", sid, err)
	}
	if !ok {
		return session.ErrNotFound
	}
	return nil
}

// ListByUser implements session.UserIndexer and omits expired rows.
func (e *Engine) ListByUser(ctx context.Context, userID string) ([]string, error) {
	now := e.now().UnixNano()
	rows, err := e.db.QueryContext(ctx, e.listSQL, e.keyArg(userID), now, now)
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
func (e *Engine) RevokeByUser(ctx context.Context, userID string, except ...string) (int, error) {
	query := fmt.Sprintf(`DELETE FROM sessions WHERE user_id = %s`, e.d.Placeholder(1)) // #nosec G201
	args := []any{e.keyArg(userID)}
	if len(except) > 0 {
		placeholders := make([]string, len(except))
		for i, sid := range except {
			placeholders[i] = e.d.Placeholder(i + 2)
			args = append(args, e.keyArg(sid))
		}
		// #nosec G202 -- builds placeholders, not user data
		query += ` AND sid NOT IN (` + strings.Join(placeholders, ",") + `)`
	}
	res, err := e.db.ExecContext(ctx, query, args...)
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
func (e *Engine) Scan(ctx context.Context, fn func(sid string) bool) error {
	now := e.now().UnixNano()
	rows, err := e.db.QueryContext(ctx, e.scanSQL, now, now)
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
func (e *Engine) Sweep(ctx context.Context) (int, error) {
	now := e.now().UnixNano()
	res, err := e.db.ExecContext(ctx, e.sweepSQL, now, now)
	if err != nil {
		return 0, fmt.Errorf("session: sweep: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// ToUnixNano maps zero time to the no-deadline marker.
func ToUnixNano(t time.Time) int64 {
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
