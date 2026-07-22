// Package storetest verifies the concurrent expiry semantics required by
// ratelimit.Store implementations.
package storetest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/ratelimit"
)

// Run tests a Store implementation; factory must return a fresh, empty store
// for each subtest.
func Run(t *testing.T, factory func(t *testing.T) ratelimit.Store) {
	t.Helper()
	base := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	t.Run("GetMissing", func(t *testing.T) {
		s := factory(t)
		if _, exists, err := s.Get(ctx, "k", base); err != nil || exists {
			t.Fatalf("exists=%v err=%v, want absent", exists, err)
		}
	})

	t.Run("SetIfAbsentThenGet", func(t *testing.T) {
		s := factory(t)
		ok, err := s.SetIfAbsent(ctx, "k", 42, base, base.Add(time.Minute))
		if err != nil || !ok {
			t.Fatalf("ok=%v err=%v", ok, err)
		}
		v, exists, err := s.Get(ctx, "k", base)
		if err != nil || !exists || v != 42 {
			t.Fatalf("v=%d exists=%v err=%v", v, exists, err)
		}
		if ok, err := s.SetIfAbsent(ctx, "k", 7, base, base.Add(time.Minute)); err != nil || ok {
			t.Fatalf("SetIfAbsent on a live entry must refuse without error: ok=%v err=%v", ok, err)
		}
		if v, exists, err := s.Get(ctx, "k", base); err != nil || !exists || v != 42 {
			t.Fatalf("refused SetIfAbsent must leave the entry intact: v=%d exists=%v err=%v", v, exists, err)
		}
	})

	t.Run("ExpiredIsAbsentEverywhere", func(t *testing.T) {
		// Use separate keys so Get cannot remove the entries tested by write
		// operations.
		s := factory(t)
		later := base.Add(time.Second)
		for _, key := range []string{"get", "cas", "set"} {
			if ok, err := s.SetIfAbsent(ctx, key, 1, base, base.Add(time.Second)); err != nil || !ok {
				t.Fatalf("seed %s: ok=%v err=%v", key, ok, err)
			}
		}
		if _, exists, err := s.Get(ctx, "get", later); err != nil || exists {
			t.Fatalf("Get on expired: exists=%v err=%v, want absent", exists, err)
		}
		if ok, err := s.CompareAndSwap(ctx, "cas", 1, 2, later, later.Add(time.Minute)); err != nil || ok {
			t.Fatalf("CompareAndSwap on expired: ok=%v err=%v, want refusal", ok, err)
		}
		ok, err := s.SetIfAbsent(ctx, "set", 9, later, later.Add(time.Minute))
		if err != nil || !ok {
			t.Fatalf("SetIfAbsent must atomically overwrite an expired entry: ok=%v err=%v", ok, err)
		}
		if v, exists, err := s.Get(ctx, "set", later); err != nil || !exists || v != 9 {
			t.Fatalf("v=%d exists=%v err=%v after overwrite", v, exists, err)
		}
	})

	t.Run("ConcurrentExpiredReplacement", func(t *testing.T) {
		s := factory(t)
		if ok, err := s.SetIfAbsent(ctx, "k", 1, base, base.Add(time.Second)); err != nil || !ok {
			t.Fatalf("seed: ok=%v err=%v", ok, err)
		}
		later := base.Add(time.Minute)
		var wg sync.WaitGroup
		var mu sync.Mutex
		wins := 0
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				ok, err := s.SetIfAbsent(ctx, "k", int64(100+i), later, later.Add(time.Minute))
				if err != nil {
					t.Error(err)
					return
				}
				if ok {
					mu.Lock()
					wins++
					mu.Unlock()
				}
			}(i)
		}
		wg.Wait()
		if wins != 1 {
			t.Fatalf("SetIfAbsent winners over an expired entry = %d, want exactly one", wins)
		}
	})

	t.Run("ConcurrentSetIfAbsentFresh", func(t *testing.T) {
		s := factory(t)
		var wg sync.WaitGroup
		var mu sync.Mutex
		wins := 0
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				ok, err := s.SetIfAbsent(ctx, "k", int64(i), base, base.Add(time.Minute))
				if err != nil {
					t.Error(err)
					return
				}
				if ok {
					mu.Lock()
					wins++
					mu.Unlock()
				}
			}(i)
		}
		wg.Wait()
		if wins != 1 {
			t.Fatalf("contended SetIfAbsent winners = %d, want exactly one", wins)
		}
	})

	t.Run("CompareAndSwap", func(t *testing.T) {
		s := factory(t)
		if ok, err := s.CompareAndSwap(ctx, "k", 0, 1, base, base.Add(time.Minute)); err != nil || ok {
			t.Fatalf("CAS on a missing key must refuse without error: ok=%v err=%v", ok, err)
		}
		setLive(t, ctx, s, "k", 1, base, base.Add(time.Minute))
		if ok, err := s.CompareAndSwap(ctx, "k", 2, 3, base, base.Add(time.Minute)); err != nil || ok {
			t.Fatalf("CAS with a stale value must refuse without error: ok=%v err=%v", ok, err)
		}
		if v, exists, err := s.Get(ctx, "k", base); err != nil || !exists || v != 1 {
			t.Fatalf("refused CAS must leave the entry intact: v=%d exists=%v err=%v", v, exists, err)
		}
		ok, err := s.CompareAndSwap(ctx, "k", 1, 3, base, base.Add(2*time.Minute))
		if err != nil || !ok {
			t.Fatalf("ok=%v err=%v", ok, err)
		}
		if v, exists, err := s.Get(ctx, "k", base); err != nil || !exists || v != 3 {
			t.Fatalf("after CAS: v=%d exists=%v err=%v", v, exists, err)
		}
		// Past the seeded expiry but inside the swapped-in one: the
		// entry is live only if CAS replaced the expiry too.
		if v, exists, err := s.Get(ctx, "k", base.Add(90*time.Second)); err != nil || !exists || v != 3 {
			t.Fatalf("CAS must replace the expiry: v=%d exists=%v err=%v", v, exists, err)
		}
	})

	t.Run("ConcurrentCASOneWinner", func(t *testing.T) {
		s := factory(t)
		setLive(t, ctx, s, "k", 10, base, base.Add(time.Minute))
		var wg sync.WaitGroup
		var mu sync.Mutex
		wins := 0
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				ok, err := s.CompareAndSwap(ctx, "k", 10, int64(100+i), base, base.Add(time.Minute))
				if err != nil {
					t.Error(err)
					return
				}
				if ok {
					mu.Lock()
					wins++
					mu.Unlock()
				}
			}(i)
		}
		wg.Wait()
		if wins != 1 {
			t.Fatalf("CAS winners = %d, want exactly one", wins)
		}
	})
}

func setLive(t *testing.T, ctx context.Context, s ratelimit.Store, key string, v int64, now, exp time.Time) {
	t.Helper()
	if ok, err := s.SetIfAbsent(ctx, key, v, now, exp); err != nil || !ok {
		t.Fatalf("seed %s: ok=%v err=%v", key, ok, err)
	}
}
