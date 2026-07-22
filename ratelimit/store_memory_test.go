package ratelimit_test

import (
	"context"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/ratelimit"
	"github.com/gofabrik/fabrik/ratelimit/storetest"
)

func TestMemoryStore_Conformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) ratelimit.Store { return ratelimit.NewMemoryStore() })
}

func TestMemoryStore_Sweep(t *testing.T) {
	s := ratelimit.NewMemoryStore()
	ctx := context.Background()
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	s.SetIfAbsent(ctx, "old", 1, now, now.Add(time.Second))
	s.SetIfAbsent(ctx, "live", 2, now, now.Add(time.Hour))
	removed := s.Sweep(ctx, now.Add(time.Minute))
	if removed != 1 {
		t.Fatalf("Sweep removed %d, want 1", removed)
	}
	if _, exists, _ := s.Get(ctx, "live", now.Add(time.Minute)); !exists {
		t.Fatal("Sweep must keep live entries")
	}
	if removed := s.Sweep(ctx, now.Add(time.Minute)); removed != 0 {
		t.Fatalf("second Sweep removed %d, want 0 (expired entry already gone)", removed)
	}
}
