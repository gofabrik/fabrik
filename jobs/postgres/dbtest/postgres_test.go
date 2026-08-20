// Package dbtest runs store conformance against a PostgreSQL-backed store.
package dbtest

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/jobs/postgres"
	"github.com/gofabrik/fabrik/jobs/storetest"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	return dsn
}

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

var pgSchemaCounter atomic.Int64

// openPostgres isolates each test through its connection search path.
func openPostgres(t *testing.T) *sql.DB {
	t.Helper()
	dsn := testDSN(t)
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, admin)
	schema := fmt.Sprintf("jobstest_%d_%d", os.Getpid(), pgSchemaCounter.Add(1))
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

// rebindPG rewrites placeholders for raw conformance queries.
func rebindPG(q string) string {
	var b strings.Builder
	n := 0
	inQuote := false
	for i := 0; i < len(q); i++ {
		c := q[i]
		switch {
		case c == '\'':
			inQuote = !inQuote
			b.WriteByte(c)
		case c == '?' && !inQuote:
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func TestPostgresStore_Conformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) storetest.Backend {
		db := openPostgres(t)
		s, err := postgres.New(db, postgres.Options{AutoCreate: true})
		if err != nil {
			t.Fatal(err)
		}
		return storetest.Backend{Store: s, DB: db, Q: rebindPG}
	})
}
