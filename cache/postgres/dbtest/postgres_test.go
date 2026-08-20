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

	"github.com/gofabrik/fabrik/cache"
	"github.com/gofabrik/fabrik/cache/postgres"
	"github.com/gofabrik/fabrik/cache/storetest"
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
	schema := fmt.Sprintf("cachetest_%d_%d", os.Getpid(), pgSchemaCounter.Add(1))
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

func newPostgresStore(t *testing.T) cache.Store {
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

func TestPostgresStore_GetDoesNotPrune(t *testing.T) {
	s := newPostgresStore(t)
	ctx := context.Background()
	now := time.Unix(5000, 0)
	exp := now.Add(-time.Minute)
	if err := s.Set(ctx, "k", cache.Entry{Value: []byte("stale"), Expires: exp}); err != nil {
		t.Fatal(err)
	}
	sw := s.(cache.Sweeper)
	for i := range 2 {
		e, ok, err := s.Get(ctx, "k", now)
		if err != nil || !ok || !e.Expires.Equal(exp) {
			t.Fatalf("read %d: %+v %v %v", i, e, ok, err)
		}
	}
	n, err := sw.Sweep(ctx, now)
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
		db, err := sql.Open("pgx", "postgres://localhost/test")
		if err != nil {
			t.Fatal(err)
		}
		db.Close()
		_, err = postgres.New(db, postgres.Options{AutoCreate: true})
		if err == nil || !strings.HasPrefix(err.Error(), "cache: create schema: ") {
			t.Fatalf("want prefix %q, got %v", "cache: create schema: ", err)
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
