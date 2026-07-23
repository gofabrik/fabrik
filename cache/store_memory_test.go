package cache_test

import (
	"context"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/cache"
	"github.com/gofabrik/fabrik/cache/storetest"
)

func TestMemoryStoreConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) cache.Store {
		return cache.NewMemoryStore(cache.MemoryOptions{})
	})
}

func TestMemoryStoreLRUEvictsOldest(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(5000, 0)
	s := cache.NewMemoryStore(cache.MemoryOptions{MaxEntries: 2})
	set := func(k string) {
		t.Helper()
		if err := s.Set(ctx, k, cache.Entry{Value: []byte(k)}); err != nil {
			t.Fatal(err)
		}
	}
	set("a")
	set("b")
	if _, ok, _ := s.Get(ctx, "a", now); !ok {
		t.Fatal("a missing")
	}
	set("c")
	if _, ok, _ := s.Get(ctx, "b", now); ok {
		t.Fatal("LRU kept the least recently used entry")
	}
	for _, k := range []string{"a", "c"} {
		if _, ok, _ := s.Get(ctx, k, now); !ok {
			t.Fatalf("%s evicted", k)
		}
	}
}

func TestMemoryStoreExpiredReadDoesNotDisplaceFresh(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(5000, 0)
	s := cache.NewMemoryStore(cache.MemoryOptions{MaxEntries: 2})
	if err := s.Set(ctx, "dead", cache.Entry{Value: []byte("d"), Expires: now.Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "fresh", cache.Entry{Value: []byte("f"), Expires: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	// Repeated expired reads must not refresh recency.
	for i := 0; i < 5; i++ {
		s.Get(ctx, "dead", now)
	}
	if err := s.Set(ctx, "new", cache.Entry{Value: []byte("n")}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Get(ctx, "fresh", now); !ok {
		t.Fatal("fresh entry displaced by a hot expired key")
	}
}

func TestMemoryStoreExpiredReadPrunes(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(5000, 0)
	s := cache.NewMemoryStore(cache.MemoryOptions{})
	exp := now.Add(-time.Second)
	if err := s.Set(ctx, "k", cache.Entry{Value: []byte("v"), Expires: exp}); err != nil {
		t.Fatal(err)
	}
	e, ok, err := s.Get(ctx, "k", now)
	if err != nil || !ok || !e.Expires.Equal(exp) {
		t.Fatalf("first expired read = %+v %v %v", e, ok, err)
	}
	if _, ok, _ := s.Get(ctx, "k", now); ok {
		t.Fatal("expired entry survived its read (prune-on-read)")
	}
}
