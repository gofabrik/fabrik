package session

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// MemoryStore is a process-local [Store] backed by a map.
//
// It implements every optional store capability. It is safe for
// concurrent use and not suitable for multi-process deployments.
type MemoryStore struct {
	mu        sync.RWMutex
	records   map[string]Record
	userIndex map[string]map[string]struct{}

	now func() time.Time // injectable for tests
}

// NewMemoryStore returns a ready-to-use in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		records:   make(map[string]Record),
		userIndex: make(map[string]map[string]struct{}),
		now:       time.Now,
	}
}

func (s *MemoryStore) Load(ctx context.Context, sid string) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	s.mu.RLock()
	rec, ok := s.records[sid]
	s.mu.RUnlock()
	if !ok {
		return Record{}, ErrNotFound
	}
	if s.expired(rec) {
		// Re-check under the write lock before pruning.
		s.mu.Lock()
		if cur, ok := s.records[sid]; ok && s.expired(cur) {
			s.deleteLocked(sid)
		}
		s.mu.Unlock()
		return Record{}, ErrNotFound
	}
	rec.Payload = clonePayload(rec.Payload)
	return rec, nil
}

func (s *MemoryStore) Save(ctx context.Context, rec Record) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	if rec.SID == "" {
		return Record{}, fmt.Errorf("memorystore: save: empty SID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	prev, exists := s.records[rec.SID]
	if exists {
		if prev.Version != rec.Version {
			return Record{}, fmt.Errorf("memorystore: sid=%s loaded=%d stored=%d: %w",
				rec.SID, rec.Version, prev.Version, ErrVersionConflict)
		}
	} else if rec.Version != 0 {
		// Revoked sessions stay revoked.
		return Record{}, fmt.Errorf("memorystore: sid=%s loaded=%d but no record present: %w",
			rec.SID, rec.Version, ErrVersionConflict)
	}

	stored := rec
	stored.Version++
	stored.Payload = clonePayload(rec.Payload)
	s.records[rec.SID] = stored

	// Keep the user index in sync with UserID changes.
	if exists && prev.UserID != "" && prev.UserID != stored.UserID {
		s.userIndexRemoveLocked(prev.UserID, rec.SID)
	}
	if stored.UserID != "" && (!exists || prev.UserID != stored.UserID) {
		s.userIndexAddLocked(stored.UserID, rec.SID)
	}

	out := stored
	out.Payload = clonePayload(stored.Payload)
	return out, nil
}

func (s *MemoryStore) Delete(ctx context.Context, sid string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteLocked(sid)
	return nil
}

// BumpTTL implements [TTLBumper] without touching Payload or Version.
func (s *MemoryStore) BumpTTL(ctx context.Context, sid string, until time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[sid]
	if !ok || s.expired(rec) {
		return ErrNotFound
	}
	rec.IdleExpiry = until
	s.records[sid] = rec
	return nil
}

// ListByUser implements [UserIndexer]. Only live sessions are returned.
func (s *MemoryStore) ListByUser(ctx context.Context, userID string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	set := s.userIndex[userID]
	out := make([]string, 0, len(set))
	for sid := range set {
		if rec, ok := s.records[sid]; ok && !s.expired(rec) {
			out = append(out, sid)
		}
	}
	return out, nil
}

// RevokeByUser implements [UserIndexer]. SIDs in except are preserved.
func (s *MemoryStore) RevokeByUser(ctx context.Context, userID string, except ...string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	keep := make(map[string]struct{}, len(except))
	for _, sid := range except {
		keep[sid] = struct{}{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	set := s.userIndex[userID]
	revoked := 0
	for sid := range set {
		if _, kept := keep[sid]; kept {
			continue
		}
		s.deleteLocked(sid)
		revoked++
	}
	return revoked, nil
}

// Scan implements [Scanner]. fn must not call back into the store.
func (s *MemoryStore) Scan(ctx context.Context, fn func(sid string) bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.RLock()
	sids := make([]string, 0, len(s.records))
	for sid, rec := range s.records {
		if s.expired(rec) {
			continue
		}
		sids = append(sids, sid)
	}
	s.mu.RUnlock()
	for _, sid := range sids {
		if !fn(sid) {
			return nil
		}
	}
	return nil
}

// Sweep implements [Sweeper]. Reads already filter expired records.
func (s *MemoryStore) Sweep(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var expired []string
	for sid, rec := range s.records {
		if s.expired(rec) {
			expired = append(expired, sid)
		}
	}
	for _, sid := range expired {
		s.deleteLocked(sid)
	}
	return len(expired), nil
}

// expired applies the record's enabled deadlines.
func (s *MemoryStore) expired(rec Record) bool {
	now := s.now()
	if !rec.AbsoluteExpiry.IsZero() && !now.Before(rec.AbsoluteExpiry) {
		return true
	}
	if !rec.IdleExpiry.IsZero() && !now.Before(rec.IdleExpiry) {
		return true
	}
	return false
}

func (s *MemoryStore) deleteLocked(sid string) {
	rec, ok := s.records[sid]
	if !ok {
		return
	}
	delete(s.records, sid)
	if rec.UserID != "" {
		s.userIndexRemoveLocked(rec.UserID, sid)
	}
}

func (s *MemoryStore) userIndexAddLocked(userID, sid string) {
	set := s.userIndex[userID]
	if set == nil {
		set = make(map[string]struct{})
		s.userIndex[userID] = set
	}
	set[sid] = struct{}{}
}

func (s *MemoryStore) userIndexRemoveLocked(userID, sid string) {
	set := s.userIndex[userID]
	if set == nil {
		return
	}
	delete(set, sid)
	if len(set) == 0 {
		delete(s.userIndex, userID)
	}
}

// clonePayload copies payload bytes across the store boundary.
func clonePayload(p []byte) []byte {
	if p == nil {
		return nil
	}
	out := make([]byte, len(p))
	copy(out, p)
	return out
}
