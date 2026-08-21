// Package postgres implements [store.Store] for PostgreSQL and serializes conflict checks with an advisory lock.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/gofabrik/fabrik/auth"
	"github.com/gofabrik/fabrik/auth/password"
	"github.com/gofabrik/fabrik/auth/store"
)

var schemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS identities (
    id         BYTEA  PRIMARY KEY,
    status     TEXT   NOT NULL,
    claims     TEXT   NOT NULL,
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL
);`,
	`CREATE TABLE IF NOT EXISTS password_credentials (
    identity_id BYTEA  NOT NULL UNIQUE REFERENCES identities(id) ON DELETE CASCADE,
    email       BYTEA  NOT NULL UNIQUE,
    hash        BYTEA  NOT NULL,
    created_at  BIGINT NOT NULL,
    updated_at  BIGINT NOT NULL
);`,
}

// advisoryKey identifies this store's write lock ("fabr", "auth").
const advisoryKey1, advisoryKey2 = 0x66616272, 0x61757468

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

// Store keeps identities and credentials in a PostgreSQL database.
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

func byteKey(s string) []byte { return []byte(s) }

// lockedTx starts a transaction and acquires the store-wide advisory lock.
func (s *Store) lockedTx(ctx context.Context) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1, $2)`, advisoryKey1, advisoryKey2); err != nil {
		tx.Rollback() //nolint:errcheck // rollback is best-effort cleanup after a failed lock
		return nil, err
	}
	return tx, nil
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
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (id) DO NOTHING`,
		byteKey(id), string(store.StatusActive), claimsText, now, now)
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
		`SELECT status, claims, created_at, updated_at FROM identities WHERE id = $1`, byteKey(id))
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
		`UPDATE identities SET status = $1, updated_at = $2 WHERE id = $3`,
		string(status), s.now().UnixNano(), byteKey(id))
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
		`UPDATE identities SET claims = $1, updated_at = $2 WHERE id = $3`,
		claimsText, s.now().UnixNano(), byteKey(id))
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
	tx, err := s.lockedTx(ctx)
	if err != nil {
		return fmt.Errorf("store: delete identity %s: %w", id, err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback is best-effort cleanup after commit or an earlier error
	if _, err := tx.ExecContext(ctx, `DELETE FROM identities WHERE id = $1`, byteKey(id)); err != nil {
		return fmt.Errorf("store: delete identity %s: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: delete identity %s: %w", id, err)
	}
	return nil
}

func (s *Store) CreatePassword(ctx context.Context, identityID, email, hash string) error {
	if !store.ValidIdentityID(identityID) {
		return store.ErrInvalid
	}
	norm, ok := store.NormalizeEmail(email)
	if !ok {
		return store.ErrInvalid
	}
	tx, err := s.lockedTx(ctx)
	if err != nil {
		return fmt.Errorf("store: create password %s: %w", identityID, err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback is best-effort cleanup after commit or an earlier error
	if err := checkPasswordWrite(ctx, tx, identityID, norm); err != nil {
		return err
	}
	var exists int
	err = tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM password_credentials WHERE identity_id = $1`, byteKey(identityID)).Scan(&exists)
	if err != nil {
		return fmt.Errorf("store: create password %s: %w", identityID, err)
	}
	if exists != 0 {
		return store.ErrExists
	}
	now := s.now().UnixNano()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO password_credentials (identity_id, email, hash, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5)`,
		byteKey(identityID), byteKey(norm), byteKey(hash), now, now); err != nil {
		return fmt.Errorf("store: create password %s: %w", identityID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: create password %s: %w", identityID, err)
	}
	return nil
}

func (s *Store) SetPassword(ctx context.Context, identityID, email, hash string) error {
	if !store.ValidIdentityID(identityID) {
		return store.ErrInvalid
	}
	norm, ok := store.NormalizeEmail(email)
	if !ok {
		return store.ErrInvalid
	}
	tx, err := s.lockedTx(ctx)
	if err != nil {
		return fmt.Errorf("store: set password %s: %w", identityID, err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback is best-effort cleanup after commit or an earlier error
	if err := checkPasswordWrite(ctx, tx, identityID, norm); err != nil {
		return err
	}
	now := s.now().UnixNano()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO password_credentials (identity_id, email, hash, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (identity_id) DO UPDATE SET
			email = excluded.email, hash = excluded.hash, updated_at = excluded.updated_at`,
		byteKey(identityID), byteKey(norm), byteKey(hash), now, now); err != nil {
		return fmt.Errorf("store: set password %s: %w", identityID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: set password %s: %w", identityID, err)
	}
	return nil
}

// Password-write conflicts are ordered ErrNotFound before ErrEmailTaken.
func checkPasswordWrite(ctx context.Context, tx *sql.Tx, identityID, norm string) error {
	var found int
	err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM identities WHERE id = $1`, byteKey(identityID)).Scan(&found)
	if err != nil {
		return fmt.Errorf("store: check identity %s: %w", identityID, err)
	}
	if found == 0 {
		return store.ErrNotFound
	}
	var owner []byte
	err = tx.QueryRowContext(ctx,
		`SELECT identity_id FROM password_credentials WHERE email = $1`, byteKey(norm)).Scan(&owner)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("store: check email owner: %w", err)
	case string(owner) != identityID:
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
		WHERE c.email = $1`, byteKey(norm))
	var (
		identityID, hash   []byte
		status, claimsText string
	)
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
		Hash: string(hash),
		Claims: auth.ClaimSet{
			Subject:      string(identityID),
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
	tx, err := s.lockedTx(ctx)
	if err != nil {
		return fmt.Errorf("store: update hash: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback is best-effort cleanup after commit or an earlier error
	row := tx.QueryRowContext(ctx, `
		SELECT c.hash, i.status
		FROM password_credentials c JOIN identities i ON i.id = c.identity_id
		WHERE c.email = $1`, byteKey(norm))
	var (
		hash   []byte
		status string
	)
	if err := row.Scan(&hash, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return password.ErrNotFound
		}
		return fmt.Errorf("store: update hash: %w", err)
	}
	if store.Status(status) != store.StatusActive {
		return password.ErrNotFound
	}
	if string(hash) != oldHash {
		return password.ErrHashChanged
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE password_credentials SET hash = $1, updated_at = $2 WHERE email = $3`,
		byteKey(newHash), s.now().UnixNano(), byteKey(norm)); err != nil {
		return fmt.Errorf("store: update hash: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: update hash: %w", err)
	}
	return nil
}
