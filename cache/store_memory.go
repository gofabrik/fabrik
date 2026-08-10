package cache

import (
	"container/list"
	"context"
	"sync"
	"time"

	"github.com/gofabrik/fabrik/cache/internal/cacheutil"
)

// MemoryStore is an in-process Store. MaxEntries caps the entry
// count; once full, the least recently used entry is evicted.
// An expired entry is returned once and pruned without refreshing its
// recency.
type MemoryStore struct {
	mu         sync.Mutex
	items      map[string]*list.Element
	lru        *list.List // front = most recently used
	maxEntries int
}

type memItem struct {
	key   string
	entry Entry
}

// MemoryOptions configures NewMemoryStore.
type MemoryOptions struct {
	// MaxEntries caps the entry count; on overflow the least recently
	// used entry is evicted. A value <= 0 means unbounded.
	MaxEntries int
}

// NewMemoryStore returns an in-process store.
func NewMemoryStore(opts MemoryOptions) *MemoryStore {
	return &MemoryStore{
		items:      make(map[string]*list.Element),
		lru:        list.New(),
		maxEntries: opts.MaxEntries,
	}
}

// Get implements Store.
func (m *MemoryStore) Get(ctx context.Context, key string, now time.Time) (Entry, bool, error) {
	if err := cacheutil.OpNow(ctx, "get", key, now); err != nil {
		return Entry{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	el, ok := m.items[key]
	if !ok {
		return Entry{}, false, nil
	}
	it := el.Value.(*memItem)
	out := Entry{Value: append([]byte(nil), it.entry.Value...), Expires: it.entry.Expires}
	if expired(it.entry, now) {
		m.remove(el)
		return out, true, nil
	}
	m.lru.MoveToFront(el)
	return out, true, nil
}

// Set implements Store.
func (m *MemoryStore) Set(ctx context.Context, key string, e Entry) error {
	if err := cacheutil.OpExpiry(ctx, "set", key, e.Expires); err != nil {
		return err
	}
	e.Value = append([]byte(nil), e.Value...)
	m.mu.Lock()
	defer m.mu.Unlock()
	if el, ok := m.items[key]; ok {
		el.Value.(*memItem).entry = e
		m.lru.MoveToFront(el)
		return nil
	}
	m.items[key] = m.lru.PushFront(&memItem{key: key, entry: e})
	if m.maxEntries > 0 && m.lru.Len() > m.maxEntries {
		if oldest := m.lru.Back(); oldest != nil {
			m.remove(oldest)
		}
	}
	return nil
}

// Delete implements Store.
func (m *MemoryStore) Delete(ctx context.Context, key string) error {
	if err := cacheutil.OpCtx(ctx, "delete", key); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if el, ok := m.items[key]; ok {
		m.remove(el)
	}
	return nil
}

// Sweep implements Sweeper.
func (m *MemoryStore) Sweep(ctx context.Context, now time.Time) (int, error) {
	if err := cacheutil.OpNow(ctx, "sweep", "", now); err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for el := m.lru.Back(); el != nil; {
		prev := el.Prev()
		if expired(el.Value.(*memItem).entry, now) {
			m.remove(el)
			n++
		}
		el = prev
	}
	return n, nil
}

func (m *MemoryStore) remove(el *list.Element) {
	m.lru.Remove(el)
	delete(m.items, el.Value.(*memItem).key)
}

// expired treats the expiry instant as expired.
func expired(e Entry, now time.Time) bool {
	return !e.Expires.IsZero() && !now.Before(e.Expires)
}
