package dbtest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	mysqldrv "github.com/go-sql-driver/mysql"
	"github.com/gofabrik/fabrik/session"
	sessionmysql "github.com/gofabrik/fabrik/session/mysql"
	"github.com/gofabrik/fabrik/session/storetest"
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
	name := fmt.Sprintf("sesstest_%d_%d", os.Getpid(), mysqlDBCounter.Add(1))
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

func newMySQLStore(t *testing.T, envVar string) session.Store {
	t.Helper()
	db := openMySQL(t, envVar)
	s, err := sessionmysql.New(db, sessionmysql.Options{AutoCreate: true})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMySQLStore_Conformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) session.Store {
		return newMySQLStore(t, "TEST_MYSQL_DSN")
	})
}

func TestMariaDBStore_Conformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) session.Store {
		return newMySQLStore(t, "TEST_MARIADB_DSN")
	})
}

func TestMySQLStore_MaxLengthSID(t *testing.T) {
	testMaxLengthSID(t, "TEST_MYSQL_DSN")
}

func TestMariaDBStore_MaxLengthSID(t *testing.T) {
	testMaxLengthSID(t, "TEST_MARIADB_DSN")
}

// testMaxLengthSID verifies the 3072-byte SID boundary without compression.
func testMaxLengthSID(t *testing.T, envVar string) {
	s := newMySQLStore(t, envVar)
	ctx := context.Background()
	sid := storetest.IncompressibleKey(3072)
	if _, err := s.Save(ctx, session.Record{
		SID:            sid,
		AbsoluteExpiry: time.Now().Add(time.Hour),
		Payload:        []byte("v"),
	}); err != nil {
		t.Fatalf("save max-length sid: %v", err)
	}
	got, err := s.Load(ctx, sid)
	if err != nil || string(got.Payload) != "v" {
		t.Fatalf("load max-length sid = %+v %v", got, err)
	}
}

func TestMySQLStore_OversizedSIDRejected(t *testing.T) {
	testOversizedSIDRejected(t, "TEST_MYSQL_DSN")
}

func TestMariaDBStore_OversizedSIDRejected(t *testing.T) {
	testOversizedSIDRejected(t, "TEST_MARIADB_DSN")
}

// testOversizedSIDRejected requires rejection instead of truncation.
func testOversizedSIDRejected(t *testing.T, envVar string) {
	s := newMySQLStore(t, envVar)
	ctx := context.Background()
	over := storetest.IncompressibleKey(3073)
	if _, err := s.Save(ctx, session.Record{
		SID:            over,
		AbsoluteExpiry: time.Now().Add(time.Hour),
		Payload:        []byte("v"),
	}); err == nil {
		t.Fatal("3073-byte sid accepted; it must be rejected")
	}
	if _, err := s.Load(ctx, over[:3072]); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("oversized sid was stored truncated to its 3072-byte prefix: %v", err)
	}
}

func TestMySQLStore_UserIDLengthBoundary(t *testing.T) {
	testUserIDLengthBoundary(t, "TEST_MYSQL_DSN")
}

func TestMariaDBStore_UserIDLengthBoundary(t *testing.T) {
	testUserIDLengthBoundary(t, "TEST_MARIADB_DSN")
}

func liveRecord(sid, userID string) session.Record {
	return session.Record{
		SID:            sid,
		UserID:         userID,
		AbsoluteExpiry: time.Now().Add(time.Hour),
		Payload:        []byte("{}"),
	}
}

// testUserIDLengthBoundary verifies acceptance at 191 bytes and rejection above it.
func testUserIDLengthBoundary(t *testing.T, envVar string) {
	s := newMySQLStore(t, envVar)
	ctx := context.Background()
	idx := s.(session.UserIndexer)
	fits := storetest.IncompressibleKey(191)
	rec := liveRecord("sid-uid-boundary", fits)
	if _, err := s.Save(ctx, rec); err != nil {
		t.Fatal(err)
	}
	sids, err := idx.ListByUser(ctx, fits)
	if err != nil || len(sids) != 1 || sids[0] != "sid-uid-boundary" {
		t.Fatalf("ListByUser(191-byte id) = %v %v", sids, err)
	}
	// Avoid sharing the accepted value's 191-byte prefix.
	over := "Z" + storetest.IncompressibleKey(191)
	if _, err := s.Save(ctx, liveRecord("sid-uid-over", over)); err == nil {
		t.Fatal("192-byte user id accepted; it must be rejected")
	}
	if sids, err := idx.ListByUser(ctx, over[:191]); err != nil {
		t.Fatal(err)
	} else if len(sids) != 0 {
		t.Fatal("oversized user id was stored truncated to its 191-byte prefix")
	}
}
