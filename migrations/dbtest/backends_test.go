package dbtest

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/migrations"
	"github.com/gofabrik/fabrik/migrations/mysql"
	"github.com/gofabrik/fabrik/migrations/postgres"
	"github.com/gofabrik/fabrik/migrations/sqlite"

	gomysql "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

// backend opens an isolated database for each test.
type backend struct {
	name        string
	driver      migrations.Driver
	mysqlFamily bool
	// serializes marks drivers that lock the whole Migrate call.
	serializes bool
	openDB     func(t *testing.T) *sql.DB
}

// backends includes live databases even when their subtests will skip.
func backends(t *testing.T) []backend {
	t.Helper()
	return []backend{
		{name: "sqlite", driver: sqlite.Driver(), openDB: openSQLite},
		{name: "postgres", driver: postgres.Driver(), serializes: true, openDB: openLiveFactory("TEST_POSTGRES_DSN", openPGFactory)},
		{name: "mysql", driver: mysql.Driver(), mysqlFamily: true, serializes: true, openDB: openLiveFactory("TEST_MYSQL_DSN", openMySQLFactory)},
		{name: "mariadb", driver: mysql.Driver(), mysqlFamily: true, serializes: true, openDB: openLiveFactory("TEST_MARIADB_DSN", openMySQLFactory)},
	}
}

// openLiveFactory defers DSN lookup so missing backends report a skip.
func openLiveFactory(envVar string, factory func(string) func(*testing.T) *sql.DB) func(*testing.T) *sql.DB {
	return func(t *testing.T) *sql.DB {
		t.Helper()
		dsn := os.Getenv(envVar)
		if dsn == "" {
			t.Skipf("%s not set", envVar)
		}
		return factory(dsn)(t)
	}
}

var isoCounter atomic.Int64

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

func openSQLite(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	// #nosec G104 -- test cleanup
	t.Cleanup(func() { db.Close() }) //nolint:errcheck
	return db
}

// openPGFactory isolates pooled connections through search_path.
func openPGFactory(dsn string) func(*testing.T) *sql.DB {
	return func(t *testing.T) *sql.DB {
		t.Helper()
		admin, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		waitReady(t, admin)
		schema := fmt.Sprintf("migtest_%d_%d", os.Getpid(), isoCounter.Add(1))
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
			// #nosec G104 -- test cleanup
			db.Close() //nolint:errcheck
			// #nosec G202 -- generated test schema identifier, not user input
			// #nosec G104 -- best-effort test teardown
			_, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE") //nolint:errcheck
			// #nosec G104 -- test cleanup
			admin.Close() //nolint:errcheck
		})
		return db
	}
}

// openMySQLFactory isolates each test while preserving required DSN options.
func openMySQLFactory(dsn string) func(*testing.T) *sql.DB {
	return func(t *testing.T) *sql.DB {
		t.Helper()
		cfg, err := gomysql.ParseDSN(dsn)
		if err != nil {
			t.Fatal(err)
		}
		admin, err := sql.Open("mysql", dsn)
		if err != nil {
			t.Fatal(err)
		}
		waitReady(t, admin)
		name := fmt.Sprintf("migtest_%d_%d", os.Getpid(), isoCounter.Add(1))
		if _, err := admin.Exec("CREATE DATABASE " + name); err != nil { // #nosec G202 -- generated test database identifier, not user input
			t.Fatal(err)
		}
		cfg.DBName = name
		db, err := sql.Open("mysql", cfg.FormatDSN())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			// #nosec G104 -- test cleanup
			db.Close() //nolint:errcheck
			// #nosec G202 -- generated test database identifier, not user input
			// #nosec G104 -- best-effort test teardown
			_, _ = admin.Exec("DROP DATABASE " + name) //nolint:errcheck
			// #nosec G104 -- test cleanup
			admin.Close() //nolint:errcheck
		})
		return db
	}
}

func countRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	// #nosec G202 -- constant test identifier, not user input
	_, err := db.Exec("SELECT 1 FROM " + name)
	return err == nil
}
