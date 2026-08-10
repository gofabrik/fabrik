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
	"strings"
	"time"

	"github.com/gofabrik/fabrik/session"
	"github.com/gofabrik/fabrik/session/internal/sqlstore"
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
	eng *sqlstore.Engine
}

// New constructs a Store and optionally applies Schema.
func New(db *sql.DB, opts Options) (*Store, error) {
	eng, err := sqlstore.New(db, mysqlDialect{}, opts.AutoCreate, opts.Now)
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

type mysqlDialect struct{}

func (mysqlDialect) Placeholder(int) string { return "?" }
func (mysqlDialect) Schema() string         { return schema }

// Insert keeps strict-mode errors fatal and reports duplicate SIDs as collisions.
func (mysqlDialect) Insert(ctx context.Context, db *sql.DB, rec session.Record) (bool, error) {
	_, err := db.ExecContext(ctx, `
		INSERT INTO sessions (sid, version, user_id, absolute_expiry, idle_expiry, payload)
		VALUES (?, 1, ?, ?, ?, ?)`,
		rec.SID, rec.UserID, sqlstore.ToUnixNano(rec.AbsoluteExpiry), sqlstore.ToUnixNano(rec.IdleExpiry), rec.Payload)
	if err == nil {
		return true, nil
	}
	if isDuplicateKey(err) {
		return false, nil
	}
	return false, err
}

// Save relies on the version increment making every matched row change.
func (mysqlDialect) Save(ctx context.Context, db *sql.DB, rec session.Record) (bool, error) {
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

// BumpTTL locks the row so an unchanged live session still reports success.
func (mysqlDialect) BumpTTL(ctx context.Context, db *sql.DB, sid string, until, now time.Time) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
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
		sqlstore.ToUnixNano(until), sid); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// isDuplicateKey recognizes error 1062 without importing a SQL driver.
func isDuplicateKey(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Error 1062")
}

var (
	_ session.Store       = (*Store)(nil)
	_ session.TTLBumper   = (*Store)(nil)
	_ session.UserIndexer = (*Store)(nil)
	_ session.Scanner     = (*Store)(nil)
	_ session.Sweeper     = (*Store)(nil)
)
