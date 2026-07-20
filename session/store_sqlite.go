package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// sqliteSchema is the idempotent DDL [SQLiteStore] expects.
const sqliteSchema = `CREATE TABLE IF NOT EXISTS sessions (
    sid             TEXT    PRIMARY KEY,
    version         INTEGER NOT NULL,
    user_id         TEXT    NOT NULL DEFAULT '',
    absolute_expiry INTEGER NOT NULL,
    idle_expiry     INTEGER NOT NULL,
    payload         BLOB    NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_user_id ON sessions(user_id) WHERE user_id <> '';
CREATE INDEX IF NOT EXISTS sessions_idle_expiry ON sessions(idle_expiry);`

// SQLiteSchema returns the DDL required to back a [SQLiteStore].
//
// The DDL is idempotent; applying it multiple times is a no-op.
func SQLiteSchema() string {
	return sqliteSchema
}

// SQLiteOptions configures a [SQLiteStore].
type SQLiteOptions struct {
	// AutoCreate runs [SQLiteSchema] during [NewSQLiteStore].
	AutoCreate bool

	// Now is the wall-clock source for expiry filtering and sweep.
	Now func() time.Time
}

// SQLiteStore is a [Store] backed by a SQLite database accessed
// through database/sql. The driver is the caller's choice.
//
// The store does not retry SQLITE_BUSY internally. Open the
// underlying *sql.DB with a busy_timeout pragma, for example
// file:db.sqlite?_pragma=busy_timeout(5000)) to absorb contention.
//
// Call [SQLiteStore.Sweep] from a scheduler to remove expired rows.
type SQLiteStore struct {
	db  *sql.DB
	now func() time.Time
}

// NewSQLiteStore wraps db with a session [Store]. When
// opts.AutoCreate is true the constructor runs [SQLiteSchema]
// against db before returning.
func NewSQLiteStore(db *sql.DB, opts SQLiteOptions) (*SQLiteStore, error) {
	if db == nil {
		return nil, errors.New("session.NewSQLiteStore: db is required")
	}
	s := &SQLiteStore{db: db, now: opts.Now}
	if s.now == nil {
		s.now = time.Now
	}
	if opts.AutoCreate {
		if _, err := db.Exec(sqliteSchema); err != nil {
			return nil, fmt.Errorf("session.NewSQLiteStore: apply schema: %w", err)
		}
	}
	return s, nil
}

func (s *SQLiteStore) Load(ctx context.Context, sid string) (Record, error) {
	now := s.now().UnixNano()
	row := s.db.QueryRowContext(ctx, `
		SELECT version, user_id, absolute_expiry, idle_expiry, payload
		FROM sessions
		WHERE sid = ?
		  AND (absolute_expiry = 0 OR absolute_expiry > ?)
		  AND (idle_expiry     = 0 OR idle_expiry     > ?)`,
		sid, now, now)

	var (
		version       uint64
		userID        string
		absExp, idExp int64
		payload       []byte
	)
	if err := row.Scan(&version, &userID, &absExp, &idExp, &payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Record{}, ErrNotFound
		}
		return Record{}, fmt.Errorf("sqlitestore: load %s: %w", sid, err)
	}
	return Record{
		SID:            sid,
		Version:        version,
		UserID:         userID,
		AbsoluteExpiry: fromUnixNano(absExp),
		IdleExpiry:     fromUnixNano(idExp),
		Payload:        payload,
	}, nil
}

func (s *SQLiteStore) Save(ctx context.Context, rec Record) (Record, error) {
	if rec.SID == "" {
		return Record{}, errors.New("sqlitestore: save: empty SID")
	}
	absExp := toUnixNano(rec.AbsoluteExpiry)
	idExp := toUnixNano(rec.IdleExpiry)

	if rec.Version == 0 {
		// Fresh inserts conflict instead of clobbering existing SIDs.
		res, err := s.db.ExecContext(ctx, `
			INSERT INTO sessions (sid, version, user_id, absolute_expiry, idle_expiry, payload)
			VALUES (?, 1, ?, ?, ?, ?)
			ON CONFLICT(sid) DO NOTHING`,
			rec.SID, rec.UserID, absExp, idExp, rec.Payload)
		if err != nil {
			return Record{}, fmt.Errorf("sqlitestore: insert %s: %w", rec.SID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return Record{}, err
		}
		if n == 0 {
			return Record{}, fmt.Errorf("sqlitestore: insert %s collided with existing row: %w", rec.SID, ErrVersionConflict)
		}
		stored := rec
		stored.Version = 1
		stored.Payload = clonePayload(rec.Payload)
		return stored, nil
	}

	res, err := s.db.ExecContext(ctx, `
		UPDATE sessions
		SET version = version + 1,
		    user_id = ?,
		    absolute_expiry = ?,
		    idle_expiry = ?,
		    payload = ?
		WHERE sid = ? AND version = ?`,
		rec.UserID, absExp, idExp, rec.Payload, rec.SID, rec.Version)
	if err != nil {
		return Record{}, fmt.Errorf("sqlitestore: update %s: %w", rec.SID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Record{}, err
	}
	if n == 0 {
		return Record{}, fmt.Errorf("sqlitestore: stale write on %s at version %d: %w", rec.SID, rec.Version, ErrVersionConflict)
	}
	stored := rec
	stored.Version++
	// Match MemoryStore's payload isolation.
	stored.Payload = clonePayload(rec.Payload)
	return stored, nil
}

func (s *SQLiteStore) Delete(ctx context.Context, sid string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE sid = ?`, sid); err != nil {
		return fmt.Errorf("sqlitestore: delete %s: %w", sid, err)
	}
	return nil
}

// BumpTTL implements [TTLBumper] without touching version or payload.
func (s *SQLiteStore) BumpTTL(ctx context.Context, sid string, until time.Time) error {
	now := s.now().UnixNano()
	res, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET idle_expiry = ?
		WHERE sid = ?
		  AND (absolute_expiry = 0 OR absolute_expiry > ?)
		  AND (idle_expiry     = 0 OR idle_expiry     > ?)`,
		toUnixNano(until), sid, now, now)
	if err != nil {
		return fmt.Errorf("sqlitestore: bump %s: %w", sid, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListByUser implements [UserIndexer]. Expired rows are filtered out.
func (s *SQLiteStore) ListByUser(ctx context.Context, userID string) ([]string, error) {
	now := s.now().UnixNano()
	rows, err := s.db.QueryContext(ctx, `
		SELECT sid FROM sessions
		WHERE user_id = ?
		  AND (absolute_expiry = 0 OR absolute_expiry > ?)
		  AND (idle_expiry     = 0 OR idle_expiry     > ?)`,
		userID, now, now)
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: list user %s: %w", userID, err)
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

// RevokeByUser implements [UserIndexer]. SIDs in except are preserved.
func (s *SQLiteStore) RevokeByUser(ctx context.Context, userID string, except ...string) (int, error) {
	query := `DELETE FROM sessions WHERE user_id = ?`
	args := []any{userID}
	if len(except) > 0 {
		placeholders := strings.Repeat("?,", len(except))
		placeholders = placeholders[:len(placeholders)-1]
		// #nosec G202 -- builds placeholders/identifiers, not user data
		query += ` AND sid NOT IN (` + placeholders + `)`
		for _, sid := range except {
			args = append(args, sid)
		}
	}
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("sqlitestore: revoke user %s: %w", userID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// Scan implements [Scanner]. Rows stream from a cursor.
func (s *SQLiteStore) Scan(ctx context.Context, fn func(sid string) bool) error {
	now := s.now().UnixNano()
	rows, err := s.db.QueryContext(ctx, `
		SELECT sid FROM sessions
		WHERE (absolute_expiry = 0 OR absolute_expiry > ?)
		  AND (idle_expiry     = 0 OR idle_expiry     > ?)`,
		now, now)
	if err != nil {
		return fmt.Errorf("sqlitestore: scan: %w", err)
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

// Sweep implements [Sweeper]. Reads already filter expired rows.
func (s *SQLiteStore) Sweep(ctx context.Context) (int, error) {
	now := s.now().UnixNano()
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM sessions
		WHERE (absolute_expiry > 0 AND absolute_expiry <= ?)
		   OR (idle_expiry     > 0 AND idle_expiry     <= ?)`,
		now, now)
	if err != nil {
		return 0, fmt.Errorf("sqlitestore: sweep: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
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
