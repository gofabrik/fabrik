package dbtest

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	mysqldrv "github.com/go-sql-driver/mysql"
	"github.com/gofabrik/fabrik/jobs"
	"github.com/gofabrik/fabrik/jobs/mysql"
	"github.com/gofabrik/fabrik/jobs/storetest"
)

var mysqlDBCounter atomic.Int64

// openMySQL gives every test its own database.
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
	name := fmt.Sprintf("jobstest_%d_%d", os.Getpid(), mysqlDBCounter.Add(1))
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

func newMySQLBackend(t *testing.T, envVar string) storetest.Backend {
	t.Helper()
	db := openMySQL(t, envVar)
	s, err := mysql.New(db, mysql.Options{AutoCreate: true})
	if err != nil {
		t.Fatal(err)
	}
	return storetest.Backend{Store: s, DB: db}
}

func TestMySQLStore_Conformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) storetest.Backend {
		return newMySQLBackend(t, "TEST_MYSQL_DSN")
	})
}

func TestMariaDBStore_Conformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) storetest.Backend {
		return newMySQLBackend(t, "TEST_MARIADB_DSN")
	})
}

func TestMySQLStore_OversizedIdentifiersRejected(t *testing.T) {
	testOversizedIdentifiersRejected(t, "TEST_MYSQL_DSN")
}

func TestMariaDBStore_OversizedIdentifiersRejected(t *testing.T) {
	testOversizedIdentifiersRejected(t, "TEST_MARIADB_DSN")
}

// testOversizedIdentifiersRejected requires rejection instead of truncation.
func testOversizedIdentifiersRejected(t *testing.T, envVar string) {
	b := newMySQLBackend(t, envVar)
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	longKind := "Z" + incompressible(255)
	if _, err := b.Store.Insert(ctx, now, []jobs.Job{{
		Kind: longKind, HandlerID: "h", Payload: []byte(`{}`),
		Queue: "default", MaxAttempts: 1, AvailableAt: now,
	}}); err == nil {
		t.Fatal("192-byte kind accepted; it must be rejected")
	}
	longUK := "Z" + incompressible(2048)
	if _, err := b.Store.Insert(ctx, now, []jobs.Job{{
		Kind: "k", HandlerID: "h", Payload: []byte(`{}`),
		Queue: "default", MaxAttempts: 1, AvailableAt: now, UniqueKey: longUK,
	}}); err == nil {
		t.Fatal("2049-byte unique key accepted; it must be rejected")
	}
}

// incompressible returns deterministic data that resists compression.
func incompressible(n int) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	out := make([]byte, n)
	state := uint64(0x9E3779B97F4A7C15)
	for i := range out {
		state = state*6364136223846793005 + 1442695040888963407
		out[i] = alphabet[state>>58]
	}
	return string(out)
}

func TestMySQLStore_IdentifierBoundaries(t *testing.T) {
	testIdentifierBoundaries(t, "TEST_MYSQL_DSN")
}

func TestMariaDBStore_IdentifierBoundaries(t *testing.T) {
	testIdentifierBoundaries(t, "TEST_MARIADB_DSN")
}

// testIdentifierBoundaries verifies the maximum accepted identifier lengths.
func testIdentifierBoundaries(t *testing.T, envVar string) {
	b := newMySQLBackend(t, envVar)
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	kind := incompressible(255)
	uk := incompressible(2048)
	res, err := b.Store.Insert(ctx, now, []jobs.Job{{
		Kind: kind, HandlerID: "h", Payload: []byte(`{}`),
		Queue: "default", MaxAttempts: 1, AvailableAt: now, UniqueKey: uk,
	}})
	if err != nil || len(res) != 1 || res[0].Duplicate {
		t.Fatalf("boundary insert: %v %+v", err, res)
	}
	info, err := b.Store.Get(ctx, res[0].ID)
	if err != nil || info.Kind != kind || info.UniqueKey != uk {
		t.Fatalf("boundary round-trip: kind ok=%v uk ok=%v err=%v", info.Kind == kind, info.UniqueKey == uk, err)
	}
}
