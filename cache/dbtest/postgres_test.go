package dbtest

import (
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
	ctx := background()
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
