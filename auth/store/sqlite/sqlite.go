// Package sqlite implements [store.Store] for SQLite; callers must enable foreign keys for cascades and should set a busy timeout because SQLITE_BUSY is not retried.
package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"time"

	"github.com/gofabrik/fabrik/auth"
	"github.com/gofabrik/fabrik/auth/password"
	"github.com/gofabrik/fabrik/auth/store"
)

var schemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS identities (
    id         TEXT    PRIMARY KEY,
    status     TEXT    NOT NULL,
    claims     TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);`,
	`CREATE TABLE IF NOT EXISTS password_credentials (
    identity_id TEXT    NOT NULL UNIQUE REFERENCES identities(id) ON DELETE CASCADE,
    email       TEXT    NOT NULL UNIQUE,
    hash        TEXT    NOT NULL,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);`,
}

// SchemaStatements returns the ordered idempotent DDL statements.
func SchemaStatements() []string {
	out := make([]string, len(schemaStatements))
	copy(out, schemaStatements)
	return out
}

// Schema returns the statements joined for migration files.
func Schema() string {
	return schemaStatements[0] + "\n" + schemaStatements[1]
}

// Options configures New.
type Options struct {
	// AutoCreate runs the schema statements during New.
	AutoCreate bool

	// Now supplies the wall clock and defaults to time.Now.
	Now func() time.Time
}

// Store keeps identities and credentials in a SQLite database.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

var (
	_ store.Store       = (*Store)(nil)
	_ password.Store    = (*Store)(nil)
	_ password.Rehasher = (*Store)(nil)
)

// New constructs a Store and optionally applies the schema.
func New(db *sql.DB, opts Options) (*Store, error) {
	if db == nil {
		return nil, errors.New("store: db is required")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	if opts.AutoCreate {
		for _, stmt := range schemaStatements {
			if _, err := db.Exec(stmt); err != nil {
				return nil, fmt.Errorf("store: create schema: %w", err)
			}
		}
	}
	return &Store{db: db, now: now}, nil
}

func (s *Store) CreateIdentity(ctx context.Context, id string, claims store.IdentityClaims) (store.Identity, error) {
	if !store.ValidIdentityID(id) {
		return store.Identity{}, store.ErrInvalid
	}
	claimsText, err := store.MarshalClaims(claims)
	if err != nil {
		return store.Identity{}, err
	}
	now := s.now().UnixNano()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO identities (id, status, claims, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		id, string(store.StatusActive), claimsText, now, now)
	if err != nil {
		return store.Identity{}, fmt.Errorf("store: create identity %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return store.Identity{}, fmt.Errorf("store: create identity %s: %w", id, err)
	}
	if n != 1 {
		return store.Identity{}, store.ErrExists
	}
	stored, err := store.UnmarshalClaims(claimsText)
	if err != nil {
		return store.Identity{}, err
	}
	return store.Identity{
		ID:        id,
		Status:    store.StatusActive,
		Claims:    stored,
		CreatedAt: time.Unix(0, now),
		UpdatedAt: time.Unix(0, now),
	}, nil
}

func (s *Store) GetIdentity(ctx context.Context, id string) (store.Identity, error) {
	if !store.ValidIdentityID(id) {
		return store.Identity{}, store.ErrInvalid
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT status, claims, created_at, updated_at FROM identities WHERE id = ?`, id)
	var (
		status, claimsText   string
		createdAt, updatedAt int64
	)
	if err := row.Scan(&status, &claimsText, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.Identity{}, store.ErrNotFound
		}
		return store.Identity{}, fmt.Errorf("store: get identity %s: %w", id, err)
	}
	claims, err := store.UnmarshalClaims(claimsText)
	if err != nil {
		return store.Identity{}, err
	}
	return store.Identity{
		ID:        id,
		Status:    store.Status(status),
		Claims:    claims,
		CreatedAt: time.Unix(0, createdAt),
		UpdatedAt: time.Unix(0, updatedAt),
	}, nil
}

func (s *Store) SetIdentityStatus(ctx context.Context, id string, status store.Status) error {
	if !store.ValidIdentityID(id) {
		return store.ErrInvalid
	}
	if status != store.StatusActive && status != store.StatusDisabled {
		return store.ErrInvalid
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE identities SET status = ?, updated_at = ? WHERE id = ?`,
		string(status), s.now().UnixNano(), id)
	if err != nil {
		return fmt.Errorf("store: set status %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: set status %s: %w", id, err)
	}
	if n != 1 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) SetIdentityClaims(ctx context.Context, id string, claims store.IdentityClaims) error {
	if !store.ValidIdentityID(id) {
		return store.ErrInvalid
	}
	claimsText, err := store.MarshalClaims(claims)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE identities SET claims = ?, updated_at = ? WHERE id = ?`,
		claimsText, s.now().UnixNano(), id)
	if err != nil {
		return fmt.Errorf("store: set claims %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: set claims %s: %w", id, err)
	}
	if n != 1 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) DeleteIdentity(ctx context.Context, id string) error {
	if !store.ValidIdentityID(id) {
		return store.ErrInvalid
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM identities WHERE id = ?`, id); err != nil {
		return fmt.Errorf("store: delete identity %s: %w", id, err)
	}
	return nil
}

// immediateTx serializes write transactions with BEGIN IMMEDIATE independent of DSN settings.
func (s *Store) immediateTx(ctx context.Context, fn func(conn *sql.Conn) error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck // returns the connection to the pool
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		// Rollback uses a fresh context after cancellation or panic, and failure evicts the connection.
		rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(rbCtx, `ROLLBACK`); err != nil {
			conn.Raw(func(any) error { return driver.ErrBadConn }) //nolint:errcheck // ErrBadConn evicts the connection
		}
	}()
	if err := fn(conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}

func wrapOp(op, id string, err error, sentinels ...error) error {
	if err == nil {
		return nil
	}
	for _, sentinel := range sentinels {
		if errors.Is(err, sentinel) {
			return err
		}
	}
	if id != "" {
		return fmt.Errorf("store: %s %s: %w", op, id, err)
	}
	return fmt.Errorf("store: %s: %w", op, err)
}

func (s *Store) CreatePassword(ctx context.Context, identityID, email, hash string) error {
	if !store.ValidIdentityID(identityID) {
		return store.ErrInvalid
	}
	norm, ok := store.NormalizeEmail(email)
	if !ok {
		return store.ErrInvalid
	}
	err := s.immediateTx(ctx, func(conn *sql.Conn) error {
		if err := checkPasswordWrite(ctx, conn, identityID, norm); err != nil {
			return err
		}
		var exists int
		err := conn.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM password_credentials WHERE identity_id = ?`, identityID).Scan(&exists)
		if err != nil {
			return err
		}
		if exists != 0 {
			return store.ErrExists
		}
		now := s.now().UnixNano()
		_, err = conn.ExecContext(ctx, `
			INSERT INTO password_credentials (identity_id, email, hash, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)`,
			identityID, norm, hash, now, now)
		return err
	})
	return wrapOp("create password", identityID, err, store.ErrNotFound, store.ErrEmailTaken, store.ErrExists)
}

func (s *Store) SetPassword(ctx context.Context, identityID, email, hash string) error {
	if !store.ValidIdentityID(identityID) {
		return store.ErrInvalid
	}
	norm, ok := store.NormalizeEmail(email)
	if !ok {
		return store.ErrInvalid
	}
	err := s.immediateTx(ctx, func(conn *sql.Conn) error {
		if err := checkPasswordWrite(ctx, conn, identityID, norm); err != nil {
			return err
		}
		now := s.now().UnixNano()
		_, err := conn.ExecContext(ctx, `
			INSERT INTO password_credentials (identity_id, email, hash, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(identity_id) DO UPDATE SET
				email = excluded.email, hash = excluded.hash, updated_at = excluded.updated_at`,
			identityID, norm, hash, now, now)
		return err
	})
	return wrapOp("set password", identityID, err, store.ErrNotFound, store.ErrEmailTaken)
}

// Password-write conflicts are ordered ErrNotFound before ErrEmailTaken.
func checkPasswordWrite(ctx context.Context, conn *sql.Conn, identityID, norm string) error {
	var found int
	err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM identities WHERE id = ?`, identityID).Scan(&found)
	if err != nil {
		return err
	}
	if found == 0 {
		return store.ErrNotFound
	}
	var owner string
	err = conn.QueryRowContext(ctx,
		`SELECT identity_id FROM password_credentials WHERE email = ?`, norm).Scan(&owner)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	case owner != identityID:
		return store.ErrEmailTaken
	}
	return nil
}

func (s *Store) Lookup(ctx context.Context, email string) (password.Credential, error) {
	norm, ok := store.NormalizeEmail(email)
	if !ok {
		return password.Credential{}, password.ErrNotFound
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT c.identity_id, c.hash, i.status, i.claims
		FROM password_credentials c JOIN identities i ON i.id = c.identity_id
		WHERE c.email = ?`, norm)
	var identityID, hash, status, claimsText string
	if err := row.Scan(&identityID, &hash, &status, &claimsText); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return password.Credential{}, password.ErrNotFound
		}
		return password.Credential{}, fmt.Errorf("store: lookup: %w", err)
	}
	if store.Status(status) != store.StatusActive {
		return password.Credential{}, password.ErrNotFound
	}
	claims, err := store.UnmarshalClaims(claimsText)
	if err != nil {
		return password.Credential{}, err
	}
	return password.Credential{
		Hash: hash,
		Claims: auth.ClaimSet{
			Subject:      identityID,
			Roles:        claims.Roles,
			Groups:       claims.Groups,
			Entitlements: claims.Entitlements,
		},
	}, nil
}

func (s *Store) UpdateHash(ctx context.Context, email, oldHash, newHash string) error {
	norm, ok := store.NormalizeEmail(email)
	if !ok {
		return password.ErrNotFound
	}
	err := s.immediateTx(ctx, func(conn *sql.Conn) error {
		row := conn.QueryRowContext(ctx, `
			SELECT c.hash, i.status
			FROM password_credentials c JOIN identities i ON i.id = c.identity_id
			WHERE c.email = ?`, norm)
		var hash, status string
		if err := row.Scan(&hash, &status); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return password.ErrNotFound
			}
			return err
		}
		if store.Status(status) != store.StatusActive {
			return password.ErrNotFound
		}
		if hash != oldHash {
			return password.ErrHashChanged
		}
		_, err := conn.ExecContext(ctx,
			`UPDATE password_credentials SET hash = ?, updated_at = ? WHERE email = ?`,
			newHash, s.now().UnixNano(), norm)
		return err
	})
	return wrapOp("update hash", "", err, password.ErrNotFound, password.ErrHashChanged)
}
