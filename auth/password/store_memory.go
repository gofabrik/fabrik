package password

import (
	"context"
	"sync"
)

// MemoryStore is a process-local Store and Rehasher for fixed account sets.
// It normalizes emails and clones credentials at its boundaries.
type MemoryStore struct {
	mu    sync.Mutex
	creds map[string]Credential
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{creds: make(map[string]Credential)}
}

func cloneCred(c Credential) Credential {
	return Credential{Hash: c.Hash, Claims: *c.Claims.Clone()}
}

// Put stores cred under the normalized email, replacing any existing value.
// The caller must hash passwords before storing them.
func (s *MemoryStore) Put(email string, cred Credential) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creds[NormalizeEmail(email)] = cloneCred(cred)
}

// Delete removes the credential for email, if any.
func (s *MemoryStore) Delete(email string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.creds, NormalizeEmail(email))
}

// Lookup returns the credential for email.
func (s *MemoryStore) Lookup(_ context.Context, email string) (Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cred, ok := s.creds[NormalizeEmail(email)]
	if !ok {
		return Credential{}, ErrNotFound
	}
	return cloneCred(cred), nil
}

// UpdateHash swaps the stored hash while it still equals oldHash.
func (s *MemoryStore) UpdateHash(_ context.Context, email, oldHash, newHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := NormalizeEmail(email)
	cred, ok := s.creds[key]
	if !ok {
		return ErrNotFound
	}
	if cred.Hash != oldHash {
		return ErrHashChanged
	}
	cred.Hash = newHash
	s.creds[key] = cred
	return nil
}
