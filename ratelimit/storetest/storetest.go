// Package storetest verifies the concurrent expiry semantics required by
// ratelimit.Store implementations.
package storetest

import (
	"context"
	"strings"
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

	t.Run("SetIfAbsentOnExpiredSameValue", func(t *testing.T) {
		// An identical expired-row overwrite still succeeds.
		s := factory(t)
		exp := base.Add(time.Second)
		later := exp.Add(time.Second)
		if ok, err := s.SetIfAbsent(ctx, "k", 42, base, exp); err != nil || !ok {
			t.Fatalf("seed: ok=%v err=%v", ok, err)
		}
		ok, err := s.SetIfAbsent(ctx, "k", 42, later, exp)
		if err != nil || !ok {
			t.Fatalf("SetIfAbsent overwriting an expired entry with identical value and expiry must succeed: ok=%v err=%v", ok, err)
		}
		if v, exists, err := s.Get(ctx, "k", base); err != nil || !exists || v != 42 {
			t.Fatalf("after identical overwrite: v=%d exists=%v err=%v", v, exists, err)
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
		for i := range 50 {
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
		for i := range 50 {
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
		setLive(ctx, t, s, "k", 1, base, base.Add(time.Minute))
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

	t.Run("CASToEqualValueSucceeds", func(t *testing.T) {
		// A matching CAS succeeds even when it changes no columns.
		s := factory(t)
		setLive(ctx, t, s, "k", 5, base, base.Add(time.Minute))
		ok, err := s.CompareAndSwap(ctx, "k", 5, 5, base, base.Add(time.Minute))
		if err != nil || !ok {
			t.Fatalf("CAS old=5 new=5 on a live entry must succeed: ok=%v err=%v", ok, err)
		}
		v, exists, err := s.Get(ctx, "k", base)
		if err != nil || !exists || v != 5 {
			t.Fatalf("after equal-value CAS: v=%d exists=%v err=%v", v, exists, err)
		}
	})

	t.Run("ConcurrentCASOneWinner", func(t *testing.T) {
		s := factory(t)
		setLive(ctx, t, s, "k", 10, base, base.Add(time.Minute))
		var wg sync.WaitGroup
		var mu sync.Mutex
		wins := 0
		for i := range 50 {
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

	t.Run("ConcurrentCASEqualValues", func(t *testing.T) {
		// Every serialized caller observes the same matching value.
		s := factory(t)
		setLive(ctx, t, s, "k", 10, base, base.Add(time.Minute))
		var wg sync.WaitGroup
		var mu sync.Mutex
		wins := 0
		for range 50 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ok, err := s.CompareAndSwap(ctx, "k", 10, 10, base, base.Add(time.Minute))
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				if ok {
					wins++
				}
			}()
		}
		wg.Wait()
		if wins != 50 {
			t.Fatalf("equal-value CAS wins = %d of 50, want all", wins)
		}
	})

	t.Run("SweepRemovesExpired", func(t *testing.T) {
		s := factory(t)
		sw, ok := s.(ratelimit.Sweeper)
		if !ok {
			t.Skip("store has no Sweeper capability")
		}
		if ok, err := s.SetIfAbsent(ctx, "old", 1, base, base.Add(time.Second)); err != nil || !ok {
			t.Fatalf("seed old: ok=%v err=%v", ok, err)
		}
		if ok, err := s.SetIfAbsent(ctx, "live", 2, base, base.Add(time.Hour)); err != nil || !ok {
			t.Fatalf("seed live: ok=%v err=%v", ok, err)
		}
		later := base.Add(time.Minute)
		n, err := sw.Sweep(ctx, later)
		if err != nil || n != 1 {
			t.Fatalf("Sweep = %d, %v; want 1, nil", n, err)
		}
		if _, exists, err := s.Get(ctx, "live", later); err != nil || !exists {
			t.Fatalf("Sweep must keep live entries (exists=%v err=%v)", exists, err)
		}
		if _, exists, err := s.Get(ctx, "old", later); err != nil || exists {
			t.Fatalf("Sweep must remove expired entries (exists=%v err=%v)", exists, err)
		}
		n, err = sw.Sweep(ctx, later)
		if err != nil || n != 0 {
			t.Fatalf("second Sweep = %d, %v; want 0, nil", n, err)
		}
	})

	t.Run("KeyBinarySafe", func(t *testing.T) {
		s := factory(t)
		for i, key := range []string{"a\x00b", "\xff\xfe"} {
			if ok, err := s.SetIfAbsent(ctx, key, int64(i+1), base, base.Add(time.Minute)); err != nil || !ok {
				t.Fatalf("SetIfAbsent binary key %q: ok=%v err=%v", key, ok, err)
			}
		}
		for i, key := range []string{"a\x00b", "\xff\xfe"} {
			v, exists, err := s.Get(ctx, key, base)
			if err != nil || !exists || v != int64(i+1) {
				t.Fatalf("Get binary key %q = %d %v %v", key, v, exists, err)
			}
		}
	})

	t.Run("TrailingSpaceKeys", func(t *testing.T) {
		s := factory(t)
		setLive(ctx, t, s, "k", 1, base, base.Add(time.Minute))
		setLive(ctx, t, s, "k ", 2, base, base.Add(time.Minute))
		v, exists, err := s.Get(ctx, "k", base)
		if err != nil || !exists || v != 1 {
			t.Fatalf("Get 'k' = %d %v %v", v, exists, err)
		}
		v, exists, err = s.Get(ctx, "k ", base)
		if err != nil || !exists || v != 2 {
			t.Fatalf("Get 'k ' = %d %v %v", v, exists, err)
		}
	})

	t.Run("CaseDistinctKeys", func(t *testing.T) {
		s := factory(t)
		setLive(ctx, t, s, "Key", 1, base, base.Add(time.Minute))
		setLive(ctx, t, s, "key", 2, base, base.Add(time.Minute))
		v, exists, err := s.Get(ctx, "Key", base)
		if err != nil || !exists || v != 1 {
			t.Fatalf("Get 'Key' = %d %v %v", v, exists, err)
		}
		v, exists, err = s.Get(ctx, "key", base)
		if err != nil || !exists || v != 2 {
			t.Fatalf("Get 'key' = %d %v %v", v, exists, err)
		}
	})

	t.Run("BoundaryLengthKey", func(t *testing.T) {
		s := factory(t)
		key := IncompressibleKey(2048)
		setLive(ctx, t, s, key, 99, base, base.Add(time.Minute))
		v, exists, err := s.Get(ctx, key, base)
		if err != nil || !exists || v != 99 {
			t.Fatalf("Get boundary key = %d %v %v", v, exists, err)
		}
	})
}

func setLive(ctx context.Context, t *testing.T, s ratelimit.Store, key string, v int64, now, exp time.Time) {
	t.Helper()
	if ok, err := s.SetIfAbsent(ctx, key, v, now, exp); err != nil || !ok {
		t.Fatalf("seed %s: ok=%v err=%v", key, ok, err)
	}
}

// IncompressibleKey returns deterministic data that resists compression.
func IncompressibleKey(n int) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	var b strings.Builder
	b.Grow(n)
	state := uint64(0x9E3779B97F4A7C15)
	for range n {
		state = state*6364136223846793005 + 1442695040888963407
		b.WriteByte(alphabet[state>>58])
	}
	return b.String()
}
