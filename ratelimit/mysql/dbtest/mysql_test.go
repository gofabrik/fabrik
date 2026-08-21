// Package dbtest runs store conformance against MySQL and MariaDB-backed stores.
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
	"github.com/gofabrik/fabrik/ratelimit"
	"github.com/gofabrik/fabrik/ratelimit/mysql"
	"github.com/gofabrik/fabrik/ratelimit/storetest"
)

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

var mysqlDBCounter atomic.Int64

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
	name := fmt.Sprintf("rltest_%d_%d", os.Getpid(), mysqlDBCounter.Add(1))
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

func newMySQLStore(t *testing.T, envVar string) ratelimit.Store {
	t.Helper()
	db := openMySQL(t, envVar)
	s, err := mysql.New(db, mysql.Options{AutoCreate: true})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMySQLStore_Conformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) ratelimit.Store {
		return newMySQLStore(t, "TEST_MYSQL_DSN")
	})
}

func TestMariaDBStore_Conformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) ratelimit.Store {
		return newMySQLStore(t, "TEST_MARIADB_DSN")
	})
}

func TestMySQLStore_Sweep(t *testing.T) {
	testSweep(t, "TEST_MYSQL_DSN")
}

func TestMariaDBStore_Sweep(t *testing.T) {
	testSweep(t, "TEST_MARIADB_DSN")
}

func testSweep(t *testing.T, envVar string) {
	s := newMySQLStore(t, envVar)
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
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	if ok, err := s.SetIfAbsent(ctx, key, 1, now, now.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("SetIfAbsent max-length key: ok=%v err=%v", ok, err)
	}
	v, exists, err := s.Get(ctx, key, now)
	if err != nil || !exists || v != 1 {
		t.Fatalf("Get max-length key = %d %v %v", v, exists, err)
	}
}

func TestMySQLStore_OversizedKeyRejected(t *testing.T) {
	testOversizedKeyRejected(t, "TEST_MYSQL_DSN")
}

func TestMariaDBStore_OversizedKeyRejected(t *testing.T) {
	testOversizedKeyRejected(t, "TEST_MARIADB_DSN")
}

// testOversizedKeyRejected requires rejection instead of truncation.
func testOversizedKeyRejected(t *testing.T, envVar string) {
	s := newMySQLStore(t, envVar)
	ctx := context.Background()
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	over := storetest.IncompressibleKey(3073)
	if ok, err := s.SetIfAbsent(ctx, over, 1, now, now.Add(time.Minute)); err == nil {
		t.Fatalf("3073-byte key accepted (ok=%v); it must be rejected", ok)
	}
	if _, exists, err := s.Get(ctx, over[:3072], now); err != nil {
		t.Fatal(err)
	} else if exists {
		t.Fatal("oversized key was stored truncated to its 3072-byte prefix")
	}
}

func TestClosedDB_ErrorWraps(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	t.Run("createSchema", func(t *testing.T) {
		db, err := sql.Open("mysql", "user:pass@tcp(127.0.0.1)/test")
		if err != nil {
			t.Fatal(err)
		}
		db.Close()
		_, err = mysql.New(db, mysql.Options{AutoCreate: true})
		if err == nil || !strings.HasPrefix(err.Error(), "ratelimit: create schema: ") {
			t.Fatalf("want prefix %q, got %v", "ratelimit: create schema: ", err)
		}
	})

	openClosed := func(t *testing.T) *mysql.Store {
		t.Helper()
		db, err := sql.Open("mysql", "user:pass@tcp(127.0.0.1)/test")
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
