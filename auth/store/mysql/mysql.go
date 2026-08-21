// Package mysql implements [store.Store] for MySQL and MariaDB with byte-for-byte comparison of IDs, emails, and hashes.
package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofabrik/fabrik/auth"
	"github.com/gofabrik/fabrik/auth/password"
	"github.com/gofabrik/fabrik/auth/store"
)

var schemaStatements = []string{
	"CREATE TABLE IF NOT EXISTS identities (\n" +
		"    id         VARBINARY(191) NOT NULL,\n" +
		"    status     VARCHAR(16)    NOT NULL,\n" +
		"    claims     LONGTEXT       NOT NULL,\n" +
		"    created_at BIGINT         NOT NULL,\n" +
		"    updated_at BIGINT         NOT NULL,\n" +
		"    PRIMARY KEY (id)\n" +
		");",
	"CREATE TABLE IF NOT EXISTS password_credentials (\n" +
		"    identity_id VARBINARY(191)  NOT NULL,\n" +
		"    email       VARBINARY(254)  NOT NULL,\n" +
		"    hash        LONGBLOB        NOT NULL,\n" +
		"    created_at  BIGINT          NOT NULL,\n" +
		"    updated_at  BIGINT          NOT NULL,\n" +
		"    UNIQUE KEY password_credentials_identity_id (identity_id),\n" +
		"    UNIQUE KEY password_credentials_email (email),\n" +
		"    CONSTRAINT password_credentials_identity_fk FOREIGN KEY (identity_id) REFERENCES identities (id) ON DELETE CASCADE\n" +
		");",
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

// Store keeps identities and credentials in a MySQL or MariaDB database.
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

// Duplicate-key classification ignores constraint names because they differ across servers.
func isDuplicateKey(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Error 1062")
}

// Foreign-key violations are detected by error number to avoid a driver dependency.
func isForeignKeyViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Error 1452")
}

func byteKey(s string) []byte { return []byte(s) }

func (s *Store) CreateIdentity(ctx context.Context, id string, claims store.IdentityClaims) (store.Identity, error) {
	if !store.ValidIdentityID(id) {
		return store.Identity{}, store.ErrInvalid
	}
	claimsText, err := store.MarshalClaims(claims)
	if err != nil {
		return store.Identity{}, err
	}
	now := s.now().UnixNano()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO identities (id, status, claims, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)`,
		byteKey(id), string(store.StatusActive), claimsText, now, now)
	if err != nil {
		if isDuplicateKey(err) {
			return store.Identity{}, store.ErrExists
		}
		return store.Identity{}, fmt.Errorf("store: create identity %s: %w", id, err)
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
		`SELECT status, claims, created_at, updated_at FROM identities WHERE id = ?`, byteKey(id))
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
	// Because MySQL counts only changed rows, read-back includes this call's timestamp to distinguish concurrent same-value writes.
	now := s.now().UnixNano()
	for range 3 {
		res, err := s.db.ExecContext(ctx,
			`UPDATE identities SET status = ?, updated_at = ? WHERE id = ?`,
			string(status), now, byteKey(id))
		if err != nil {
			return fmt.Errorf("store: set status %s: %w", id, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: set status %s: %w", id, err)
		}
		if n != 0 {
			return nil
		}
		var (
			current   string
			updatedAt int64
		)
		err = s.db.QueryRowContext(ctx,
			`SELECT status, updated_at FROM identities WHERE id = ?`, byteKey(id)).Scan(&current, &updatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: set status %s: %w", id, err)
		}
		if store.Status(current) == status && updatedAt == now {
			return nil
		}
	}
	return fmt.Errorf("store: set status %s: update raced repeatedly", id)
}

func (s *Store) SetIdentityClaims(ctx context.Context, id string, claims store.IdentityClaims) error {
	if !store.ValidIdentityID(id) {
		return store.ErrInvalid
	}
	claimsText, err := store.MarshalClaims(claims)
	if err != nil {
		return err
	}
	now := s.now().UnixNano()
	for range 3 {
		res, err := s.db.ExecContext(ctx,
			`UPDATE identities SET claims = ?, updated_at = ? WHERE id = ?`,
			claimsText, now, byteKey(id))
		if err != nil {
			return fmt.Errorf("store: set claims %s: %w", id, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: set claims %s: %w", id, err)
		}
		if n != 0 {
			return nil
		}
		var (
			current   string
			updatedAt int64
		)
		err = s.db.QueryRowContext(ctx,
			`SELECT claims, updated_at FROM identities WHERE id = ?`, byteKey(id)).Scan(&current, &updatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: set claims %s: %w", id, err)
		}
		if current == claimsText && updatedAt == now {
			return nil
		}
	}
	return fmt.Errorf("store: set claims %s: update raced repeatedly", id)
}

func (s *Store) DeleteIdentity(ctx context.Context, id string) error {
	if !store.ValidIdentityID(id) {
		return store.ErrInvalid
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM identities WHERE id = ?`, byteKey(id)); err != nil {
		return fmt.Errorf("store: delete identity %s: %w", id, err)
	}
	return nil
}

// Password-write conflicts are ordered ErrNotFound before ErrEmailTaken.
func (s *Store) checkPasswordWrite(ctx context.Context, op, identityID, norm string) error {
	var found int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM identities WHERE id = ?`, byteKey(identityID)).Scan(&found)
	if err != nil {
		return fmt.Errorf("store: %s %s: %w", op, identityID, err)
	}
	if found == 0 {
		return store.ErrNotFound
	}
	var owner []byte
	err = s.db.QueryRowContext(ctx,
		`SELECT identity_id FROM password_credentials WHERE email = ?`, byteKey(norm)).Scan(&owner)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("store: %s %s: %w", op, identityID, err)
	case string(owner) != identityID:
		return store.ErrEmailTaken
	}
	return nil
}

// CreatePassword adds a credential and rechecks conflicts after a duplicate-key error.
func (s *Store) CreatePassword(ctx context.Context, identityID, email, hash string) error {
	if !store.ValidIdentityID(identityID) {
		return store.ErrInvalid
	}
	norm, ok := store.NormalizeEmail(email)
	if !ok {
		return store.ErrInvalid
	}
	for range 3 {
		if err := s.checkPasswordWrite(ctx, "create password", identityID, norm); err != nil {
			return err
		}
		var exists int
		err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM password_credentials WHERE identity_id = ?`, byteKey(identityID)).Scan(&exists)
		if err != nil {
			return fmt.Errorf("store: create password %s: %w", identityID, err)
		}
		if exists != 0 {
			return store.ErrExists
		}
		now := s.now().UnixNano()
		_, err = s.db.ExecContext(ctx, `
			INSERT INTO password_credentials (identity_id, email, hash, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)`,
			byteKey(identityID), byteKey(norm), byteKey(hash), now, now)
		switch {
		case err == nil:
			return nil
		case isDuplicateKey(err):
			continue
		case isForeignKeyViolation(err):
			return store.ErrNotFound
		default:
			return fmt.Errorf("store: create password %s: %w", identityID, err)
		}
	}
	return fmt.Errorf("store: create password %s: conflict raced repeatedly", identityID)
}

func (s *Store) SetPassword(ctx context.Context, identityID, email, hash string) error {
	if !store.ValidIdentityID(identityID) {
		return store.ErrInvalid
	}
	norm, ok := store.NormalizeEmail(email)
	if !ok {
		return store.ErrInvalid
	}
	now := s.now().UnixNano()
	errRaced := errors.New("raced")
	attempt := func() error {
		if err := s.checkPasswordWrite(ctx, "set password", identityID, norm); err != nil {
			return err
		}
		res, err := s.db.ExecContext(ctx,
			`UPDATE password_credentials SET email = ?, hash = ?, updated_at = ? WHERE identity_id = ?`,
			byteKey(norm), byteKey(hash), now, byteKey(identityID))
		if err != nil {
			if isDuplicateKey(err) {
				return errRaced
			}
			return fmt.Errorf("store: set password %s: %w", identityID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: set password %s: %w", identityID, err)
		}
		if n != 0 {
			return nil
		}
		// MySQL counts changed rows only; a matched identical row reads back.
		var same int
		err = s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM password_credentials WHERE identity_id = ? AND email = ? AND hash = ?`,
			byteKey(identityID), byteKey(norm), byteKey(hash)).Scan(&same)
		if err != nil {
			return fmt.Errorf("store: set password %s: %w", identityID, err)
		}
		if same != 0 {
			return nil
		}
		_, err = s.db.ExecContext(ctx, `
			INSERT INTO password_credentials (identity_id, email, hash, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)`,
			byteKey(identityID), byteKey(norm), byteKey(hash), now, now)
		switch {
		case err == nil:
			return nil
		case isDuplicateKey(err):
			return errRaced
		case isForeignKeyViolation(err):
			return store.ErrNotFound
		default:
			return fmt.Errorf("store: set password %s: %w", identityID, err)
		}
	}
	for range 3 {
		err := attempt()
		if errors.Is(err, errRaced) {
			continue
		}
		return err
	}
	return fmt.Errorf("store: set password %s: conflict raced repeatedly", identityID)
}

func (s *Store) Lookup(ctx context.Context, email string) (password.Credential, error) {
	norm, ok := store.NormalizeEmail(email)
	if !ok {
		return password.Credential{}, password.ErrNotFound
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT c.identity_id, c.hash, i.status, i.claims
		FROM password_credentials c JOIN identities i ON i.id = c.identity_id
		WHERE c.email = ?`, byteKey(norm))
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
	res, err := s.db.ExecContext(ctx, `
		UPDATE password_credentials c JOIN identities i ON i.id = c.identity_id
		SET c.hash = ?, c.updated_at = ?
		WHERE c.email = ? AND c.hash = ? AND i.status = ?`,
		byteKey(newHash), s.now().UnixNano(), byteKey(norm), byteKey(oldHash), string(store.StatusActive))
	if err != nil {
		return fmt.Errorf("store: update hash: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: update hash: %w", err)
	}
	if n != 0 {
		return nil
	}
	// A zero count may mean an identical hash, so classify it by read-back.
	row := s.db.QueryRowContext(ctx, `
		SELECT c.hash, i.status
		FROM password_credentials c JOIN identities i ON i.id = c.identity_id
		WHERE c.email = ?`, byteKey(norm))
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
	// An already-stored newHash is successful only when oldHash equals newHash.
	if oldHash == newHash && string(hash) == newHash {
		return nil
	}
	return password.ErrHashChanged
}
