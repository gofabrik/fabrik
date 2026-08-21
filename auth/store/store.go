// Package store defines identity and password-credential persistence compatible with [password.Store] and [password.Rehasher].
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gofabrik/fabrik/auth/password"
)

var (
	// ErrNotFound reports an unknown identity; Lookup and UpdateHash return [password.ErrNotFound] instead.
	ErrNotFound = errors.New("store: identity not found")

	// ErrExists reports a duplicate identity or password credential.
	ErrExists = errors.New("store: already exists")

	// ErrEmailTaken reports an email owned by another identity and takes precedence over [ErrExists].
	ErrEmailTaken = errors.New("store: email belongs to another identity")

	// ErrInvalid reports an invalid identity ID, email, or status before storage access.
	ErrInvalid = errors.New("store: invalid input")
)

// MaxIdentityIDLen limits identity IDs to the session user ID capacity, in bytes.
const MaxIdentityIDLen = 191

// ValidIdentityID reports whether id is non-empty and at most MaxIdentityIDLen bytes.
func ValidIdentityID(id string) bool {
	return id != "" && len(id) <= MaxIdentityIDLen
}

// NormalizeEmail applies [password.NormalizeEmail] and reports whether the result is non-empty and within [password.MaxEmailLen].
func NormalizeEmail(email string) (string, bool) {
	e := password.NormalizeEmail(email)
	return e, e != "" && len(e) <= password.MaxEmailLen
}

type claimsJSON struct {
	Roles        []string `json:"roles,omitempty"`
	Groups       []string `json:"groups,omitempty"`
	Entitlements []string `json:"entitlements,omitempty"`
}

// MarshalClaims encodes claims for SQL storage.
func MarshalClaims(c IdentityClaims) (string, error) {
	b, err := json.Marshal(claimsJSON{Roles: c.Roles, Groups: c.Groups, Entitlements: c.Entitlements})
	if err != nil {
		return "", fmt.Errorf("store: encode claims: %w", err)
	}
	return string(b), nil
}

// UnmarshalClaims decodes a persisted claims column value.
func UnmarshalClaims(text string) (IdentityClaims, error) {
	var c claimsJSON
	if err := json.Unmarshal([]byte(text), &c); err != nil {
		return IdentityClaims{}, fmt.Errorf("store: decode claims: %w", err)
	}
	return IdentityClaims{Roles: c.Roles, Groups: c.Groups, Entitlements: c.Entitlements}, nil
}

// Status controls future logins without affecting existing sessions.
type Status string

const (
	StatusActive   Status = "active"
	StatusDisabled Status = "disabled"
)

// IdentityClaims contains persisted authorization claims, excluding grant-specific token and request claims.
type IdentityClaims struct {
	Roles        []string
	Groups       []string
	Entitlements []string
}

// Identity represents an account and its authorization state.
type Identity struct {
	ID     string
	Status Status
	Claims IdentityClaims

	// CreatedAt is immutable; UpdatedAt changes only with status or claims.
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Store persists identities and password credentials and implements [password.Store] and [password.Rehasher].
type Store interface {
	// CreateIdentity creates an active identity under an opaque ID of at most MaxIdentityIDLen bytes.
	CreateIdentity(ctx context.Context, id string, claims IdentityClaims) (Identity, error)

	// GetIdentity returns the identity regardless of status.
	GetIdentity(ctx context.Context, id string) (Identity, error)

	// SetIdentityStatus switches logins on or off for the identity.
	SetIdentityStatus(ctx context.Context, id string, status Status) error

	// SetIdentityClaims replaces the identity-owned claims.
	SetIdentityClaims(ctx context.Context, id string, claims IdentityClaims) error

	// DeleteIdentity removes an identity and its credentials and succeeds if the identity is absent.
	DeleteIdentity(ctx context.Context, id string) error

	// CreatePassword adds a credential without replacing one; conflicts are ordered ErrNotFound, ErrEmailTaken, then ErrExists.
	CreatePassword(ctx context.Context, identityID, email, hash string) error

	// SetPassword replaces an identity's email and hash, returning ErrEmailTaken if another identity owns the email.
	SetPassword(ctx context.Context, identityID, email, hash string) error

	// Lookup implements [password.Store], treating invalid input and disabled identities as [password.ErrNotFound].
	Lookup(ctx context.Context, email string) (password.Credential, error)

	// UpdateHash implements [password.Rehasher], returning [password.ErrHashChanged] on a compare-and-swap miss.
	UpdateHash(ctx context.Context, email, oldHash, newHash string) error
}
