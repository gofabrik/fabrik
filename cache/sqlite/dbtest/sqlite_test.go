// Package dbtest runs store conformance against a SQLite-backed store.
package dbtest

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/cache"
	"github.com/gofabrik/fabrik/cache/sqlite"
	"github.com/gofabrik/fabrik/cache/storetest"
	_ "modernc.org/sqlite"
)

func openSQLite(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cache.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})
	return db
}

func newSQLiteStore(t *testing.T) *sqlite.Store {
	t.Helper()
	s, err := sqlite.New(openSQLite(t), sqlite.Options{AutoCreate: true})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSQLiteStore_Conformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) cache.Store {
		return newSQLiteStore(t)
	})
}

func TestSQLiteStore_AutoCreate(t *testing.T) {
	db := openSQLite(t)
	if _, err := sqlite.New(db, sqlite.Options{}); err != nil {
		t.Fatal(err)
	}
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'cache_entries'`).Scan(&n)
	if err != nil || n != 0 {
		t.Fatalf("table exists without AutoCreate: n=%d err=%v", n, err)
	}
	if _, err := db.Exec(sqlite.Schema()); err != nil {
		t.Fatalf("applying Schema: %v", err)
	}
	if _, err := db.Exec(sqlite.Schema()); err != nil {
		t.Fatalf("Schema is not idempotent: %v", err)
	}
}

func TestSQLiteStore_NoExpiryStoredAsNull(t *testing.T) {
	db := openSQLite(t)
	s, err := sqlite.New(db, sqlite.Options{AutoCreate: true})
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
	s := newSQLiteStore(t)
	ctx := context.Background()
	now := time.Unix(5000, 0)
	exp := now.Add(-time.Minute)
	if err := s.Set(ctx, "k", cache.Entry{Value: []byte("stale"), Expires: exp}); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		e, ok, err := s.Get(ctx, "k", now)
		if err != nil || !ok || !e.Expires.Equal(exp) {
			t.Fatalf("read %d: %+v %v %v", i, e, ok, err)
		}
	}
	n, err := s.Sweep(ctx, now)
	if err != nil || n != 1 {
		t.Fatalf("Sweep = %d, %v", n, err)
	}
	if _, ok, err := s.Get(ctx, "k", now); err != nil || ok {
		t.Fatalf("swept row: ok=%v err=%v", ok, err)
	}
}

func TestClosedDB_ErrorWraps(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	t.Run("createSchema", func(t *testing.T) {
		db, err := sql.Open("sqlite", "file::memory:")
		if err != nil {
			t.Fatal(err)
		}
		db.Close()
		_, err = sqlite.New(db, sqlite.Options{AutoCreate: true})
		if err == nil || !strings.HasPrefix(err.Error(), "cache: create schema: ") {
			t.Fatalf("want prefix %q, got %v", "cache: create schema: ", err)
		}
	})

	openClosed := func(t *testing.T) *sqlite.Store {
		t.Helper()
		db, err := sql.Open("sqlite", "file::memory:")
		if err != nil {
			t.Fatal(err)
		}
		s, err := sqlite.New(db, sqlite.Options{})
		if err != nil {
			t.Fatal(err)
		}
		db.Close()
		return s
	}

	t.Run("get", func(t *testing.T) {
		s := openClosed(t)
		_, _, err := s.Get(ctx, "k", now)
		if err == nil || !strings.HasPrefix(err.Error(), `cache: get "k": `) {
			t.Fatalf("want prefix %q, got %v", `cache: get "k": `, err)
		}
	})

	t.Run("set", func(t *testing.T) {
		s := openClosed(t)
		err := s.Set(ctx, "k", cache.Entry{Value: []byte("v")})
		if err == nil || !strings.HasPrefix(err.Error(), `cache: set "k": `) {
			t.Fatalf("want prefix %q, got %v", `cache: set "k": `, err)
		}
	})

	t.Run("delete", func(t *testing.T) {
		s := openClosed(t)
		err := s.Delete(ctx, "k")
		if err == nil || !strings.HasPrefix(err.Error(), `cache: delete "k": `) {
			t.Fatalf("want prefix %q, got %v", `cache: delete "k": `, err)
		}
	})

	t.Run("sweep", func(t *testing.T) {
		s := openClosed(t)
		_, err := s.Sweep(ctx, now)
		if err == nil || !strings.HasPrefix(err.Error(), "cache: sweep: ") {
			t.Fatalf("want prefix %q, got %v", "cache: sweep: ", err)
		}
	})
}
