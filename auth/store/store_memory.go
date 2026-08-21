package store

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/gofabrik/fabrik/auth"
	"github.com/gofabrik/fabrik/auth/password"
)

// MemoryOptions configures a [MemoryStore].
type MemoryOptions struct {
	// Now supplies the wall clock and defaults to time.Now.
	Now func() time.Time
}

// MemoryStore is the in-process reference implementation of [Store].
type MemoryStore struct {
	now func() time.Time

	mu           sync.Mutex
	identities   map[string]*memIdentity
	credsByID    map[string]*memCredential
	credsByEmail map[string]*memCredential
}

type memIdentity struct {
	status    Status
	claims    IdentityClaims
	createdAt int64
	updatedAt int64
}

type memCredential struct {
	identityID string
	email      string
	hash       string
}

var (
	_ Store             = (*MemoryStore)(nil)
	_ password.Store    = (*MemoryStore)(nil)
	_ password.Rehasher = (*MemoryStore)(nil)
)

// NewMemoryStore returns an empty store.
func NewMemoryStore(opts MemoryOptions) *MemoryStore {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &MemoryStore{
		now:          now,
		identities:   make(map[string]*memIdentity),
		credsByID:    make(map[string]*memCredential),
		credsByEmail: make(map[string]*memCredential),
	}
}

func cloneIdentityClaims(c IdentityClaims) IdentityClaims {
	return IdentityClaims{
		Roles:        slices.Clone(c.Roles),
		Groups:       slices.Clone(c.Groups),
		Entitlements: slices.Clone(c.Entitlements),
	}
}

func (m *memIdentity) identity(id string) Identity {
	return Identity{
		ID:        id,
		Status:    m.status,
		Claims:    cloneIdentityClaims(m.claims),
		CreatedAt: time.Unix(0, m.createdAt),
		UpdatedAt: time.Unix(0, m.updatedAt),
	}
}

func (s *MemoryStore) CreateIdentity(ctx context.Context, id string, claims IdentityClaims) (Identity, error) {
	if !ValidIdentityID(id) {
		return Identity{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.identities[id]; ok {
		return Identity{}, ErrExists
	}
	now := s.now().UnixNano()
	ident := &memIdentity{
		status:    StatusActive,
		claims:    cloneIdentityClaims(claims),
		createdAt: now,
		updatedAt: now,
	}
	s.identities[id] = ident
	return ident.identity(id), nil
}

func (s *MemoryStore) GetIdentity(ctx context.Context, id string) (Identity, error) {
	if !ValidIdentityID(id) {
		return Identity{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ident, ok := s.identities[id]
	if !ok {
		return Identity{}, ErrNotFound
	}
	return ident.identity(id), nil
}

func (s *MemoryStore) SetIdentityStatus(ctx context.Context, id string, status Status) error {
	if !ValidIdentityID(id) {
		return ErrInvalid
	}
	if status != StatusActive && status != StatusDisabled {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ident, ok := s.identities[id]
	if !ok {
		return ErrNotFound
	}
	ident.status = status
	ident.updatedAt = s.now().UnixNano()
	return nil
}

func (s *MemoryStore) SetIdentityClaims(ctx context.Context, id string, claims IdentityClaims) error {
	if !ValidIdentityID(id) {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ident, ok := s.identities[id]
	if !ok {
		return ErrNotFound
	}
	ident.claims = cloneIdentityClaims(claims)
	ident.updatedAt = s.now().UnixNano()
	return nil
}

func (s *MemoryStore) DeleteIdentity(ctx context.Context, id string) error {
	if !ValidIdentityID(id) {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.identities, id)
	if cred, ok := s.credsByID[id]; ok {
		delete(s.credsByEmail, cred.email)
		delete(s.credsByID, id)
	}
	return nil
}

func (s *MemoryStore) CreatePassword(ctx context.Context, identityID, email, hash string) error {
	if !ValidIdentityID(identityID) {
		return ErrInvalid
	}
	norm, ok := NormalizeEmail(email)
	if !ok {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.identities[identityID]; !ok {
		return ErrNotFound
	}
	if owner, ok := s.credsByEmail[norm]; ok && owner.identityID != identityID {
		return ErrEmailTaken
	}
	if _, ok := s.credsByID[identityID]; ok {
		return ErrExists
	}
	cred := &memCredential{identityID: identityID, email: norm, hash: hash}
	s.credsByID[identityID] = cred
	s.credsByEmail[norm] = cred
	return nil
}

func (s *MemoryStore) SetPassword(ctx context.Context, identityID, email, hash string) error {
	if !ValidIdentityID(identityID) {
		return ErrInvalid
	}
	norm, ok := NormalizeEmail(email)
	if !ok {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.identities[identityID]; !ok {
		return ErrNotFound
	}
	if owner, ok := s.credsByEmail[norm]; ok && owner.identityID != identityID {
		return ErrEmailTaken
	}
	if old, ok := s.credsByID[identityID]; ok {
		delete(s.credsByEmail, old.email)
	}
	cred := &memCredential{identityID: identityID, email: norm, hash: hash}
	s.credsByID[identityID] = cred
	s.credsByEmail[norm] = cred
	return nil
}

// activeCredential returns a credential only for a valid email belonging to an active identity.
func (s *MemoryStore) activeCredential(email string) (*memCredential, *memIdentity, bool) {
	norm, ok := NormalizeEmail(email)
	if !ok {
		return nil, nil, false
	}
	cred, ok := s.credsByEmail[norm]
	if !ok {
		return nil, nil, false
	}
	ident, ok := s.identities[cred.identityID]
	if !ok || ident.status != StatusActive {
		return nil, nil, false
	}
	return cred, ident, true
}

func (s *MemoryStore) Lookup(ctx context.Context, email string) (password.Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cred, ident, ok := s.activeCredential(email)
	if !ok {
		return password.Credential{}, password.ErrNotFound
	}
	claims := cloneIdentityClaims(ident.claims)
	return password.Credential{
		Hash: cred.hash,
		Claims: auth.ClaimSet{
			Subject:      cred.identityID,
			Roles:        claims.Roles,
			Groups:       claims.Groups,
			Entitlements: claims.Entitlements,
		},
	}, nil
}

func (s *MemoryStore) UpdateHash(ctx context.Context, email, oldHash, newHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cred, _, ok := s.activeCredential(email)
	if !ok {
		return password.ErrNotFound
	}
	if cred.hash != oldHash {
		return password.ErrHashChanged
	}
	cred.hash = newHash
	return nil
}
