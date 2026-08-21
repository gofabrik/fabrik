// Package dbtest runs store conformance against a PostgreSQL-backed store.
package dbtest

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/ratelimit"
	"github.com/gofabrik/fabrik/ratelimit/postgres"
	"github.com/gofabrik/fabrik/ratelimit/storetest"
	_ "github.com/jackc/pgx/v5/stdlib"
)

var pgSchemaCounter atomic.Int64

func waitReady(t *testing.T, db *sql.DB) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var err error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err = db.PingContext(ctx)
		cancel()
		if err == nil {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("database not ready: %v", err)
}

// openPostgres isolates each test through its connection search path.
func openPostgres(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, admin)
	schema := fmt.Sprintf("rltest_%d_%d", os.Getpid(), pgSchemaCounter.Add(1))
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil { // #nosec G202 -- generated test schema identifier, not user input
		t.Fatal(err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := sql.Open("pgx", dsn+sep+"options="+url.QueryEscape("-csearch_path="+schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
		if _, err := admin.Exec("DROP SCHEMA " + schema + " CASCADE"); err != nil { // #nosec G202 -- generated test schema identifier, not user input
			t.Errorf("drop test schema: %v", err)
		}
		if err := admin.Close(); err != nil {
			t.Errorf("close admin connection: %v", err)
		}
	})
	return db
}

func newPostgresStore(t *testing.T) ratelimit.Store {
	t.Helper()
	db := openPostgres(t)
	s, err := postgres.New(db, postgres.Options{AutoCreate: true})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPostgresStore_Conformance(t *testing.T) {
	storetest.Run(t, newPostgresStore)
}

func TestPostgresStore_Sweep(t *testing.T) {
	s := newPostgresStore(t)
	sw := s.(ratelimit.Sweeper)
	ctx := context.Background()
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	if ok, err := s.SetIfAbsent(ctx, "old", 1, now, now.Add(time.Second)); err != nil || !ok {
		t.Fatalf("seed old: ok=%v err=%v", ok, err)
	}
	if ok, err := s.SetIfAbsent(ctx, "live", 2, now, now.Add(time.Hour)); err != nil || !ok {
		t.Fatalf("seed live: ok=%v err=%v", ok, err)
	}
	removed, err := sw.Sweep(ctx, now.Add(time.Minute))
	if err != nil || removed != 1 {
		t.Fatalf("Sweep = %d, %v; want 1, nil", removed, err)
	}
	if _, exists, err := s.Get(ctx, "live", now.Add(time.Minute)); err != nil || !exists {
		t.Fatalf("Sweep must keep live entries (exists=%v err=%v)", exists, err)
	}
}

func TestPostgresStore_KeyLengthBoundary(t *testing.T) {
	// Probe away from PostgreSQL's content-dependent B-tree boundary.
	s := newPostgresStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	under := storetest.IncompressibleKey(2600)
	if ok, err := s.SetIfAbsent(ctx, under, 1, now, now.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("SetIfAbsent 2600-byte key: ok=%v err=%v", ok, err)
	}
	if v, exists, err := s.Get(ctx, under, now); err != nil || !exists || v != 1 {
		t.Fatalf("Get 2600-byte key = %d %v %v", v, exists, err)
	}
	over := storetest.IncompressibleKey(2800)
	if _, err := s.SetIfAbsent(ctx, over, 1, now, now.Add(time.Minute)); err == nil {
		t.Fatal("2800-byte incompressible key accepted; the documented boundary is stale")
	}
}

func TestClosedDB_ErrorWraps(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	t.Run("createSchema", func(t *testing.T) {
		db, err := sql.Open("pgx", "postgres://localhost/test")
		if err != nil {
			t.Fatal(err)
		}
		db.Close()
		_, err = postgres.New(db, postgres.Options{AutoCreate: true})
		if err == nil || !strings.HasPrefix(err.Error(), "ratelimit: create schema: ") {
			t.Fatalf("want prefix %q, got %v", "ratelimit: create schema: ", err)
		}
	})

	openClosed := func(t *testing.T) *postgres.Store {
		t.Helper()
		db, err := sql.Open("pgx", "postgres://localhost/test")
		if err != nil {
			t.Fatal(err)
		}
		s, err := postgres.New(db, postgres.Options{})
		if err != nil {
			t.Fatal(err)
		}
		db.Close()
		return s
	}

	t.Run("get", func(t *testing.T) {
		s := openClosed(t)
		_, _, err := s.Get(ctx, "k", now)
		if err == nil || !strings.HasPrefix(err.Error(), `ratelimit: get "k": `) {
			t.Fatalf("want prefix %q, got %v", `ratelimit: get "k": `, err)
		}
	})

	t.Run("setAbsent", func(t *testing.T) {
		s := openClosed(t)
		_, err := s.SetIfAbsent(ctx, "k", 1, now, now.Add(time.Minute))
		if err == nil || !strings.HasPrefix(err.Error(), `ratelimit: set absent "k": `) {
			t.Fatalf("want prefix %q, got %v", `ratelimit: set absent "k": `, err)
		}
	})

	t.Run("swap", func(t *testing.T) {
		s := openClosed(t)
		_, err := s.CompareAndSwap(ctx, "k", 1, 2, now, now.Add(time.Minute))
		if err == nil || !strings.HasPrefix(err.Error(), `ratelimit: swap "k": `) {
			t.Fatalf("want prefix %q, got %v", `ratelimit: swap "k": `, err)
		}
	})

	t.Run("sweep", func(t *testing.T) {
		s := openClosed(t)
		_, err := s.Sweep(ctx, now)
		if err == nil || !strings.HasPrefix(err.Error(), "ratelimit: sweep: ") {
			t.Fatalf("want prefix %q, got %v", "ratelimit: sweep: ", err)
		}
	})
}
