// Package sqlite is a batteries-included [password.Store] over a
// SQLite database: it owns the users table, ID generation, and email
// normalization, so an app integrating password auth writes none of
// them. Bring your own [password.Store] only if you have a
// nonstandard user table.
//
//	store, err := sqlite.New(db, sqlite.Options{AutoCreate: true})
//
// AutoCreate applies the schema during New; leave it false and manage
// the table through a migration tool via [Schema] instead - the same
// choice session's SQLite store offers, so both stores treat schema
// the same way. The driver is the caller's choice - the store only
// sees *sql.DB. Open the DB with a busy_timeout pragma to absorb
// writer contention.
package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/gofabrik/fabrik/auth/password"
)

// schema is the DDL the store expects; idempotent, applied on New.
const schema = `CREATE TABLE IF NOT EXISTS auth_users (
    id        TEXT PRIMARY KEY,
    email     TEXT NOT NULL UNIQUE,
    pass_hash BLOB NOT NULL
)`

// Schema returns the DDL backing the store, for apps that manage
// schema through a migration tool instead of [Options.AutoCreate].
func Schema() string { return schema }

// Options configures the store. The zero value manages no schema: the
// table must already exist, applied by a migration running [Schema].
type Options struct {
	// AutoCreate applies [Schema] during [New] (idempotent).
	// Convenient for a small app; leave it false when a migration
	// tool owns the schema, matching session.SQLiteOptions.AutoCreate.
	AutoCreate bool
}

// Store is a SQLite-backed [password.Store].
type Store struct {
	db *sql.DB
}

// New wraps db as a [password.Store]. With Options.AutoCreate it
// applies the schema; otherwise it assumes the table already exists.
// It is the whole store: LookupByEmail, Create, ID generation, and
// email normalization are all here.
func New(db *sql.DB, opts Options) (*Store, error) {
	if db == nil {
		return nil, errors.New("authsqlite.New: nil db")
	}
	if opts.AutoCreate {
		if _, err := db.Exec(schema); err != nil {
			return nil, fmt.Errorf("authsqlite.New: apply schema: %w", err)
		}
	}
	return &Store{db: db}, nil
}

func (s *Store) LookupByEmail(ctx context.Context, email string) (password.User, error) {
	var u password.User
	err := s.db.QueryRowContext(ctx,
		"SELECT id, email, pass_hash FROM auth_users WHERE email = ?", normalize(email)).
		Scan(&u.ID, &u.Email, &u.PassHash)
	if errors.Is(err, sql.ErrNoRows) {
		return password.User{}, password.ErrUserNotFound
	}
	if err != nil {
		return password.User{}, fmt.Errorf("authsqlite: lookup: %w", err)
	}
	return u, nil
}

func (s *Store) Create(ctx context.Context, email string, passHash []byte) (password.User, error) {
	id, err := newID()
	if err != nil {
		return password.User{}, err
	}
	email = normalize(email)
	_, err = s.db.ExecContext(ctx,
		"INSERT INTO auth_users (id, email, pass_hash) VALUES (?, ?, ?)", id, email, passHash)
	if err != nil {
		// Scope the match to the email constraint: any other UNIQUE
		// violation (an id collision, a column an integrator added)
		// is a genuine fault, not a taken email, and must surface.
		if strings.Contains(err.Error(), "auth_users.email") {
			return password.User{}, password.ErrEmailTaken
		}
		return password.User{}, fmt.Errorf("authsqlite: create: %w", err)
	}
	return password.User{ID: id, Email: email, PassHash: passHash}, nil
}

// normalize is the canonical email form: trim then lowercase, so
// case and whitespace variants are one account. Consistent across
// lookup, create, and the UNIQUE constraint.
func normalize(email string) string {
	return strings.TrimSpace(strings.ToLower(email))
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
