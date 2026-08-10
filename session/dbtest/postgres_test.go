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

	"github.com/gofabrik/fabrik/session"
	sessionpostgres "github.com/gofabrik/fabrik/session/postgres"
	"github.com/gofabrik/fabrik/session/storetest"
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
	schema := fmt.Sprintf("sesstest_%d_%d", os.Getpid(), pgSchemaCounter.Add(1))
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

func newPostgresStore(t *testing.T) session.Store {
	t.Helper()
	db := openPostgres(t)
	s, err := sessionpostgres.New(db, sessionpostgres.Options{AutoCreate: true})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPostgresStore_Conformance(t *testing.T) {
	storetest.Run(t, newPostgresStore)
}

func TestPostgresStore_SIDLengthBoundary(t *testing.T) {
	// Probe away from PostgreSQL's content-dependent B-tree boundary.
	s := newPostgresStore(t)
	ctx := context.Background()
	under := storetest.IncompressibleKey(2600)
	if _, err := s.Save(ctx, session.Record{
		SID:            under,
		AbsoluteExpiry: time.Now().Add(time.Hour),
		Payload:        []byte("v"),
	}); err != nil {
		t.Fatalf("save 2600-byte SID: %v", err)
	}
	if got, err := s.Load(ctx, under); err != nil || string(got.Payload) != "v" {
		t.Fatalf("load 2600-byte SID = %+v %v", got, err)
	}
	over := storetest.IncompressibleKey(2800)
	if _, err := s.Save(ctx, session.Record{
		SID:            over,
		AbsoluteExpiry: time.Now().Add(time.Hour),
		Payload:        []byte("v"),
	}); err == nil {
		t.Fatal("2800-byte incompressible SID accepted; the documented boundary is stale")
	}
}
