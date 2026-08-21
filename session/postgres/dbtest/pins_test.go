// Closed handles fail before dialing, so these tests require no server.
package dbtest

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/session"
	sessionpostgres "github.com/gofabrik/fabrik/session/postgres"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func openClosedDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", "postgres://closed:closed@localhost:1/closed")
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func openClosedStore(t *testing.T) *sessionpostgres.Store {
	t.Helper()
	db := openClosedDB(t)
	s, err := sessionpostgres.New(db, sessionpostgres.Options{})
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	return s
}

func TestClosedDB_ErrorWraps(t *testing.T) {
	ctx := context.Background()

	t.Run("createSchema", func(t *testing.T) {
		db := openClosedDB(t)
		db.Close()
		_, err := sessionpostgres.New(db, sessionpostgres.Options{AutoCreate: true})
		wantPrefix(t, err, "session: create schema: ")
	})
	t.Run("load", func(t *testing.T) {
		s := openClosedStore(t)
		_, err := s.Load(ctx, "k")
		wantPrefix(t, err, "session: load k: ")
	})
	t.Run("insert", func(t *testing.T) {
		s := openClosedStore(t)
		_, err := s.Save(ctx, session.Record{SID: "k"})
		wantPrefix(t, err, "session: insert k: ")
	})
	t.Run("update", func(t *testing.T) {
		s := openClosedStore(t)
		_, err := s.Save(ctx, session.Record{SID: "k", Version: 1})
		wantPrefix(t, err, "session: update k: ")
	})
	t.Run("delete", func(t *testing.T) {
		s := openClosedStore(t)
		wantPrefix(t, s.Delete(ctx, "k"), "session: delete k: ")
	})
	t.Run("bump", func(t *testing.T) {
		s := openClosedStore(t)
		wantPrefix(t, s.BumpTTL(ctx, "k", time.Now().Add(time.Hour)), "session: bump k: ")
	})
	t.Run("listUser", func(t *testing.T) {
		s := openClosedStore(t)
		_, err := s.ListByUser(ctx, "u")
		wantPrefix(t, err, "session: list user u: ")
	})
	t.Run("revokeUser", func(t *testing.T) {
		s := openClosedStore(t)
		_, err := s.RevokeByUser(ctx, "u")
		wantPrefix(t, err, "session: revoke user u: ")
	})
	t.Run("scan", func(t *testing.T) {
		s := openClosedStore(t)
		wantPrefix(t, s.Scan(ctx, func(string) bool { return true }), "session: scan: ")
	})
	t.Run("sweep", func(t *testing.T) {
		s := openClosedStore(t)
		_, err := s.Sweep(ctx)
		wantPrefix(t, err, "session: sweep: ")
	})
}

func wantPrefix(t *testing.T, err error, prefix string) {
	t.Helper()
	if err == nil || !strings.HasPrefix(err.Error(), prefix) {
		t.Fatalf("want prefix %q, got %v", prefix, err)
	}
}
