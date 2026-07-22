// Package dbtest runs store conformance against driver-backed stores.
package dbtest

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/ratelimit"
	"github.com/gofabrik/fabrik/ratelimit/storetest"
	_ "modernc.org/sqlite"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rl.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestSQLiteStore_Conformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) ratelimit.Store {
		s, err := ratelimit.NewSQLiteStore(openDB(t), ratelimit.SQLiteOptions{AutoCreate: true})
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

func TestSQLiteStore_AutoCreate(t *testing.T) {
	db := openDB(t)
	s, err := ratelimit.NewSQLiteStore(db, ratelimit.SQLiteOptions{})
	if err != nil {
		t.Fatalf("construction without AutoCreate must succeed (schema is the caller's job): %v", err)
	}
	if _, _, err := s.Get(context.Background(), "k", time.Now()); err == nil {
		t.Fatal("operations without schema must error, not succeed silently")
	}
	if _, err := db.Exec(ratelimit.SQLiteSchema()); err != nil {
		t.Fatalf("caller-applied schema: %v", err)
	}
	if _, err := db.Exec(ratelimit.SQLiteSchema()); err != nil {
		t.Fatalf("schema must be idempotent: %v", err)
	}
	if _, _, err := s.Get(context.Background(), "k", time.Now()); err != nil {
		t.Fatalf("Get after caller-applied schema: %v", err)
	}
	if _, err := ratelimit.NewSQLiteStore(nil, ratelimit.SQLiteOptions{}); err == nil {
		t.Fatal("nil db accepted")
	}
}

func TestSQLiteStore_Sweep(t *testing.T) {
	s, err := ratelimit.NewSQLiteStore(openDB(t), ratelimit.SQLiteOptions{AutoCreate: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	if ok, err := s.SetIfAbsent(ctx, "old", 1, now, now.Add(time.Second)); err != nil || !ok {
		t.Fatalf("seed old: ok=%v err=%v", ok, err)
	}
	if ok, err := s.SetIfAbsent(ctx, "live", 2, now, now.Add(time.Hour)); err != nil || !ok {
		t.Fatalf("seed live: ok=%v err=%v", ok, err)
	}
	removed, err := s.Sweep(ctx, now.Add(time.Minute))
	if err != nil || removed != 1 {
		t.Fatalf("Sweep = %d, %v; want 1 removed", removed, err)
	}
	if _, exists, err := s.Get(ctx, "live", now.Add(time.Minute)); err != nil || !exists {
		t.Fatalf("Sweep must keep live entries (exists=%v err=%v)", exists, err)
	}
	if removed, err := s.Sweep(ctx, now.Add(time.Minute)); err != nil || removed != 0 {
		t.Fatalf("second Sweep = %d, %v; want 0, nil", removed, err)
	}
}

func TestSQLiteStore_WorksWithLimiter(t *testing.T) {
	s, err := ratelimit.NewSQLiteStore(openDB(t), ratelimit.SQLiteOptions{AutoCreate: true})
	if err != nil {
		t.Fatal(err)
	}
	lim, err := ratelimit.New(ratelimit.PerMinute(6).WithBurst(2), s,
		ratelimit.WithClock(func() time.Time { return time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC) }))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if res, err := lim.Allow(ctx, "k"); err != nil || !res.Allowed {
			t.Fatalf("call %d: %+v err=%v", i+1, res, err)
		}
	}
	res, err := lim.Allow(ctx, "k")
	if err != nil || res.Allowed {
		t.Fatalf("third call must deny: %+v err=%v", res, err)
	}
	if res.RetryAfter != 10*time.Second {
		t.Fatalf("RetryAfter = %s, want exactly 10s through SQL state", res.RetryAfter)
	}
}
