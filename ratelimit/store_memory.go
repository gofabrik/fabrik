package ratelimit

import (
	"context"
	"sync"
	"time"
)

// MemoryStore is a process-local Store that limits each replica independently
// and expires entries lazily; call Sweep periodically to reclaim idle keys.
type MemoryStore struct {
	mu sync.Mutex
	m  map[string]memEntry
}

type memEntry struct {
	value     int64
	expiresAt time.Time
}

// NewMemoryStore returns an empty in-process store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{m: map[string]memEntry{}} }

func (s *MemoryStore) Get(ctx context.Context, key string, now time.Time) (int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[key]
	if !ok {
		return 0, false, nil
	}
	if !e.expiresAt.After(now) {
		delete(s.m, key)
		return 0, false, nil
	}
	return e.value, true, nil
}

func (s *MemoryStore) SetIfAbsent(ctx context.Context, key string, value int64, now, expiresAt time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.m[key]; ok && e.expiresAt.After(now) {
		return false, nil
	}
	s.m[key] = memEntry{value: value, expiresAt: expiresAt}
	return true, nil
}

func (s *MemoryStore) CompareAndSwap(ctx context.Context, key string, old, new int64, now, expiresAt time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[key]
	if !ok || !e.expiresAt.After(now) || e.value != old {
		return false, nil
	}
	s.m[key] = memEntry{value: new, expiresAt: expiresAt}
	return true, nil
}

// Sweep removes every entry expired at now and reports how many.
func (s *MemoryStore) Sweep(ctx context.Context, now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for k, e := range s.m {
		if !e.expiresAt.After(now) {
			delete(s.m, k)
			removed++
		}
	}
	return removed
}
