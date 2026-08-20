// Package dbtest runs store conformance against a MySQL/MariaDB-backed store.
package dbtest

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mysqldrv "github.com/go-sql-driver/mysql"
	"github.com/gofabrik/fabrik/cache"
	"github.com/gofabrik/fabrik/cache/mysql"
	"github.com/gofabrik/fabrik/cache/storetest"
)

var mysqlDBCounter atomic.Int64

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

// openMySQL isolates each test in its own database.
func openMySQL(t *testing.T, envVar string) *sql.DB {
	t.Helper()
	dsn := os.Getenv(envVar)
	if dsn == "" {
		t.Skipf("%s not set", envVar)
	}
	cfg, err := mysqldrv.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, admin)
	name := fmt.Sprintf("cachetest_%d_%d", os.Getpid(), mysqlDBCounter.Add(1))
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil { // #nosec G202 -- generated test database identifier, not user input
		t.Fatal(err)
	}
	cfg.DBName = name
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
		if _, err := admin.Exec("DROP DATABASE " + name); err != nil { // #nosec G202 -- generated test database identifier, not user input
			t.Errorf("drop test database: %v", err)
		}
		if err := admin.Close(); err != nil {
			t.Errorf("close admin connection: %v", err)
		}
	})
	return db
}

func newMySQLStore(t *testing.T, envVar string) cache.Store {
	t.Helper()
	db := openMySQL(t, envVar)
	s, err := mysql.New(db, mysql.Options{AutoCreate: true})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMySQLStore_Conformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) cache.Store {
		return newMySQLStore(t, "TEST_MYSQL_DSN")
	})
}

func TestMariaDBStore_Conformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) cache.Store {
		return newMySQLStore(t, "TEST_MARIADB_DSN")
	})
}

func TestMySQLStore_GetDoesNotPrune(t *testing.T) {
	testGetDoesNotPrune(t, "TEST_MYSQL_DSN")
}

func TestMariaDBStore_GetDoesNotPrune(t *testing.T) {
	testGetDoesNotPrune(t, "TEST_MARIADB_DSN")
}

func testGetDoesNotPrune(t *testing.T, envVar string) {
	s := newMySQLStore(t, envVar)
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

func TestMySQLStore_MaxLengthKey(t *testing.T) {
	testMaxLengthKey(t, "TEST_MYSQL_DSN")
}

func TestMariaDBStore_MaxLengthKey(t *testing.T) {
	testMaxLengthKey(t, "TEST_MARIADB_DSN")
}

// testMaxLengthKey verifies the 3072-byte key boundary without compression.
func testMaxLengthKey(t *testing.T, envVar string) {
	s := newMySQLStore(t, envVar)
	ctx := context.Background()
	key := storetest.IncompressibleKey(3072)
	if err := s.Set(ctx, key, cache.Entry{Value: []byte("v")}); err != nil {
		t.Fatal(err)
	}
	e, ok, err := s.Get(ctx, key, time.Unix(5000, 0))
	if err != nil || !ok || string(e.Value) != "v" {
		t.Fatalf("Get max-length key = %q %v %v", e.Value, ok, err)
	}
}

func TestClosedDB_ErrorWraps(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	t.Run("createSchema", func(t *testing.T) {
		db, err := sql.Open("mysql", "root@/test")
		if err != nil {
			t.Fatal(err)
		}
		db.Close()
		_, err = mysql.New(db, mysql.Options{AutoCreate: true})
		if err == nil || !strings.HasPrefix(err.Error(), "cache: create schema: ") {
			t.Fatalf("want prefix %q, got %v", "cache: create schema: ", err)
		}
	})

	openClosed := func(t *testing.T) *mysql.Store {
		t.Helper()
		db, err := sql.Open("mysql", "root@/test")
		if err != nil {
			t.Fatal(err)
		}
		s, err := mysql.New(db, mysql.Options{})
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
