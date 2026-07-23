// Package dbtest runs store conformance against driver-backed stores.
package dbtest

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/cache"
	"github.com/gofabrik/fabrik/cache/storetest"
	_ "modernc.org/sqlite"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cache.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newStore(t *testing.T) *cache.SQLiteStore {
	t.Helper()
	s, err := cache.NewSQLiteStore(openDB(t), cache.SQLiteOptions{AutoCreate: true})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSQLiteStore_Conformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) cache.Store {
		return newStore(t)
	})
}

func TestSQLiteStore_AutoCreate(t *testing.T) {
	db := openDB(t)
	if _, err := cache.NewSQLiteStore(db, cache.SQLiteOptions{}); err != nil {
		t.Fatal(err)
	}
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'cache_entries'`).Scan(&n)
	if err != nil || n != 0 {
		t.Fatalf("table exists without AutoCreate: n=%d err=%v", n, err)
	}
	if _, err := db.Exec(cache.SQLiteSchema()); err != nil {
		t.Fatalf("applying SQLiteSchema: %v", err)
	}
	if _, err := db.Exec(cache.SQLiteSchema()); err != nil {
		t.Fatalf("SQLiteSchema is not idempotent: %v", err)
	}
}

func TestSQLiteStore_NoExpiryStoredAsNull(t *testing.T) {
	db := openDB(t)
	s, err := cache.NewSQLiteStore(db, cache.SQLiteOptions{AutoCreate: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.Set(ctx, "forever", cache.Entry{Value: []byte("v")}); err != nil {
		t.Fatal(err)
	}
	var nulls int
	if err := db.QueryRow(`SELECT COUNT(*) FROM cache_entries WHERE expires_at IS NULL`).Scan(&nulls); err != nil {
		t.Fatal(err)
	}
	if nulls != 1 {
		t.Fatalf("no-expiry entry not stored as NULL (count=%d)", nulls)
	}
	// The Unix epoch is an expiry, not the no-expiry sentinel.
	if err := s.Set(ctx, "epoch", cache.Entry{Value: []byte("v"), Expires: time.Unix(0, 0)}); err != nil {
		t.Fatal(err)
	}
	var zeroes int
	if err := db.QueryRow(`SELECT COUNT(*) FROM cache_entries WHERE expires_at = 0`).Scan(&zeroes); err != nil {
		t.Fatal(err)
	}
	if zeroes != 1 {
		t.Fatalf("epoch expiry not stored as 0 (count=%d)", zeroes)
	}
}

func TestSQLiteStore_GetDoesNotPrune(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	now := time.Unix(5000, 0)
	exp := now.Add(-time.Minute)
	if err := s.Set(ctx, "k", cache.Entry{Value: []byte("stale"), Expires: exp}); err != nil {
		t.Fatal(err)
	}
	// Reads leave expired rows for Sweep.
	for i := 0; i < 2; i++ {
		e, ok, err := s.Get(ctx, "k", now)
		if err != nil || !ok || !e.Expires.Equal(exp) {
			t.Fatalf("read %d: %+v %v %v", i, e, ok, err)
		}
	}
	n, err := s.Sweep(ctx, now)
	if err != nil || n != 1 {
		t.Fatalf("Sweep = %d, %v", n, err)
	}
	if _, ok, _ := s.Get(ctx, "k", now); ok {
		t.Fatal("swept row still present")
	}
}
