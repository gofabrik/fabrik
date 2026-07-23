// Package storetest verifies cache.Store implementations.
//
//	func TestMyStore(t *testing.T) {
//		storetest.Run(t, func(t *testing.T) cache.Store {
//			return NewMyStore(t)
//		})
//	}
package storetest

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/cache"
)

var (
	minInstant = time.Unix(0, math.MinInt64)
	maxInstant = time.Unix(0, math.MaxInt64)
)

// Run checks a Store and its optional Sweeper.
func Run(t *testing.T, factory func(t *testing.T) cache.Store) {
	ctx := context.Background()
	now := time.Unix(5000, 0)

	t.Run("RoundTripPreservesValueAndExpiry", func(t *testing.T) {
		s := factory(t)
		exp := now.Add(time.Minute)
		set(t, s, "k", cache.Entry{Value: []byte("v"), Expires: exp})
		e, ok, err := s.Get(ctx, "k", now)
		if err != nil || !ok {
			t.Fatalf("Get = %v %v", ok, err)
		}
		if string(e.Value) != "v" || !e.Expires.Equal(exp) {
			t.Fatalf("entry = %q %v, want v %v", e.Value, e.Expires, exp)
		}
	})

	t.Run("ZeroExpiryMeansNever", func(t *testing.T) {
		s := factory(t)
		set(t, s, "k", cache.Entry{Value: []byte("v")})
		e, ok, err := s.Get(ctx, "k", maxInstant)
		if err != nil || !ok || !e.Expires.IsZero() {
			t.Fatalf("no-expiry entry: %v %v %v", e.Expires, ok, err)
		}
	})

	t.Run("NilValueRoundTrips", func(t *testing.T) {
		s := factory(t)
		set(t, s, "k", cache.Entry{Value: nil})
		e, ok, err := s.Get(ctx, "k", now)
		if err != nil || !ok || len(e.Value) != 0 {
			t.Fatalf("nil-value entry = %+v %v %v", e, ok, err)
		}
	})

	t.Run("MissIsFalseNil", func(t *testing.T) {
		s := factory(t)
		e, ok, err := s.Get(ctx, "absent", now)
		if err != nil || ok || e.Value != nil {
			t.Fatalf("miss = %+v %v %v", e, ok, err)
		}
	})

	t.Run("OverwriteReplaces", func(t *testing.T) {
		s := factory(t)
		set(t, s, "k", cache.Entry{Value: []byte("old"), Expires: now.Add(time.Minute)})
		set(t, s, "k", cache.Entry{Value: []byte("new")})
		e, ok, err := s.Get(ctx, "k", now)
		if err != nil || !ok || string(e.Value) != "new" || !e.Expires.IsZero() {
			t.Fatalf("overwrite = %+v %v %v", e, ok, err)
		}
	})

	t.Run("DeleteIsIdempotent", func(t *testing.T) {
		s := factory(t)
		set(t, s, "k", cache.Entry{Value: []byte("v")})
		if err := s.Delete(ctx, "k"); err != nil {
			t.Fatal(err)
		}
		if err := s.Delete(ctx, "k"); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := s.Get(ctx, "k", now); ok {
			t.Fatal("deleted key present")
		}
	})

	t.Run("ValuesDoNotAlias", func(t *testing.T) {
		s := factory(t)
		in := []byte("abc")
		set(t, s, "k", cache.Entry{Value: in})
		in[0] = 'X'
		e, _, _ := s.Get(ctx, "k", now)
		if string(e.Value) != "abc" {
			t.Fatalf("caller mutation reached the store: %q", e.Value)
		}
		e.Value[0] = 'Y'
		e2, _, _ := s.Get(ctx, "k", now)
		if string(e2.Value) != "abc" {
			t.Fatalf("returned-slice mutation reached the store: %q", e2.Value)
		}
	})

	t.Run("ExpiredEntryIsReturnedOrGoneNeverCorrupted", func(t *testing.T) {
		s := factory(t)
		exp := now.Add(-time.Minute)
		set(t, s, "k", cache.Entry{Value: []byte("stale"), Expires: exp})
		e, ok, err := s.Get(ctx, "k", now)
		if err != nil {
			t.Fatal(err)
		}
		// An expired entry is either exact or already pruned.
		if ok && (string(e.Value) != "stale" || !e.Expires.Equal(exp)) {
			t.Fatalf("expired entry corrupted: %+v", e)
		}
	})

	t.Run("EpochExpiryRoundTrips", func(t *testing.T) {
		s := factory(t)
		epoch := time.Unix(0, 0)
		set(t, s, "k", cache.Entry{Value: []byte("v"), Expires: epoch})
		// The Unix epoch must not collide with the zero-time sentinel.
		e, ok, err := s.Get(ctx, "k", minInstant)
		if err != nil || !ok {
			t.Fatalf("epoch-expiring entry lost: %v %v", ok, err)
		}
		if string(e.Value) != "v" || !e.Expires.Equal(epoch) || e.Expires.IsZero() {
			t.Fatalf("epoch expiry read back as %+v", e)
		}
	})

	t.Run("SweepAcceptsDomainBoundaries", func(t *testing.T) {
		s := factory(t)
		sw, ok := s.(cache.Sweeper)
		if !ok {
			t.Skip("store has no Sweeper capability")
		}
		epoch := time.Unix(0, 0)
		set(t, s, "k", cache.Entry{Value: []byte("v"), Expires: epoch})
		n, err := sw.Sweep(ctx, minInstant)
		if err != nil || n != 0 {
			t.Fatalf("Sweep(min) = %d, %v; entry expiring at epoch is not yet expired", n, err)
		}
		n, err = sw.Sweep(ctx, epoch)
		if err != nil || n != 1 {
			t.Fatalf("Sweep(epoch) = %d, %v; the expiry instant itself is expired", n, err)
		}
		if _, err := sw.Sweep(ctx, maxInstant); err != nil {
			t.Fatalf("Sweep(max): %v", err)
		}
	})

	t.Run("ExpiryDomainBoundaries", func(t *testing.T) {
		s := factory(t)
		for _, tt := range []time.Time{minInstant, maxInstant} {
			if err := s.Set(ctx, "k", cache.Entry{Value: []byte("v"), Expires: tt}); err != nil {
				t.Fatalf("boundary %v rejected: %v", tt, err)
			}
		}
		for _, tt := range []time.Time{minInstant.Add(-time.Nanosecond), maxInstant.Add(time.Nanosecond)} {
			if err := s.Set(ctx, "k", cache.Entry{Value: []byte("v"), Expires: tt}); err == nil {
				t.Fatalf("out-of-domain expiry %v accepted", tt)
			}
		}
	})

	t.Run("OutOfDomainNowRejected", func(t *testing.T) {
		s := factory(t)
		set(t, s, "k", cache.Entry{Value: []byte("v")})
		for _, bad := range []time.Time{minInstant.Add(-time.Nanosecond), maxInstant.Add(time.Nanosecond), {}} {
			if _, _, err := s.Get(ctx, "k", bad); err == nil {
				t.Fatalf("Get accepted out-of-domain now %v", bad)
			}
			if sw, ok := s.(cache.Sweeper); ok {
				if _, err := sw.Sweep(ctx, bad); err == nil {
					t.Fatalf("Sweep accepted out-of-domain now %v", bad)
				}
			}
		}
	})

	t.Run("PreCanceledContextRefused", func(t *testing.T) {
		s := factory(t)
		set(t, s, "k", cache.Entry{Value: []byte("v")})
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if _, _, err := s.Get(canceled, "k", now); err == nil {
			t.Fatal("Get accepted canceled ctx")
		}
		if err := s.Set(canceled, "k", cache.Entry{Value: []byte("v")}); err == nil {
			t.Fatal("Set accepted canceled ctx")
		}
		if err := s.Delete(canceled, "k"); err == nil {
			t.Fatal("Delete accepted canceled ctx")
		}
		if sw, ok := s.(cache.Sweeper); ok {
			if _, err := sw.Sweep(canceled, now); err == nil {
				t.Fatal("Sweep accepted canceled ctx")
			}
		}
	})

	t.Run("SweepRemovesExactlyExpired", func(t *testing.T) {
		s := factory(t)
		sw, ok := s.(cache.Sweeper)
		if !ok {
			t.Skip("store has no Sweeper capability")
		}
		set(t, s, "dead1", cache.Entry{Value: []byte("v"), Expires: now.Add(-time.Second)})
		set(t, s, "dead2", cache.Entry{Value: []byte("v"), Expires: now.Add(-time.Hour)})
		set(t, s, "alive", cache.Entry{Value: []byte("v"), Expires: now.Add(time.Hour)})
		set(t, s, "forever", cache.Entry{Value: []byte("v")})
		n, err := sw.Sweep(ctx, now)
		if err != nil || n != 2 {
			t.Fatalf("Sweep = %d, %v; want 2", n, err)
		}
		for _, k := range []string{"alive", "forever"} {
			if _, ok, _ := s.Get(ctx, k, now); !ok {
				t.Fatalf("sweep removed unexpired key %s", k)
			}
		}
		for _, k := range []string{"dead1", "dead2"} {
			if _, ok, _ := s.Get(ctx, k, now); ok {
				t.Fatalf("sweep left expired key %s", k)
			}
		}
	})

	t.Run("ConcurrentSetsDistinctKeys", func(t *testing.T) {
		s := factory(t)
		var wg sync.WaitGroup
		errs := make(chan error, 20)
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				k := string(rune('a' + i))
				if err := s.Set(ctx, k, cache.Entry{Value: []byte(k)}); err != nil {
					errs <- err
				}
			}(i)
		}
		wg.Wait()
		close(errs)
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 20; i++ {
			k := string(rune('a' + i))
			e, ok, err := s.Get(ctx, k, now)
			if err != nil || !ok || string(e.Value) != k {
				t.Fatalf("key %s = %q %v %v", k, e.Value, ok, err)
			}
		}
	})
}

func set(t *testing.T, s cache.Store, key string, e cache.Entry) {
	t.Helper()
	if err := s.Set(context.Background(), key, e); err != nil {
		t.Fatalf("Set %q: %v", key, err)
	}
}
