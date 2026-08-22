// Closed handles fail before dialing, so these tests require no server.
package dbtest

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/session"
	sessionsqlite "github.com/gofabrik/fabrik/session/sqlite"

	_ "modernc.org/sqlite"
)

// markerSID and markerUserID are distinctive enough that a prefix
// match alone cannot hide their presence elsewhere in the error text.
const (
	markerSID    = "closed-db-marker-sid-4e2a"
	markerUserID = "closed-db-marker-user-7c91"
)

func openClosedDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func openClosedStore(t *testing.T) *sessionsqlite.Store {
	t.Helper()
	db := openClosedDB(t)
	s, err := sessionsqlite.New(db, sessionsqlite.Options{})
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
		_, err := sessionsqlite.New(db, sessionsqlite.Options{AutoCreate: true})
		wantPrefix(t, err, "session: create schema: ")
	})
	t.Run("load", func(t *testing.T) {
		s := openClosedStore(t)
		_, err := s.Load(ctx, markerSID)
		wantCredentialFree(t, err, "session: load: ", markerSID)
	})
	t.Run("insert", func(t *testing.T) {
		s := openClosedStore(t)
		_, err := s.Save(ctx, session.Record{SID: markerSID})
		wantCredentialFree(t, err, "session: insert: ", markerSID)
	})
	t.Run("update", func(t *testing.T) {
		s := openClosedStore(t)
		_, err := s.Save(ctx, session.Record{SID: markerSID, Version: 1})
		wantCredentialFree(t, err, "session: update: ", markerSID)
	})
	t.Run("delete", func(t *testing.T) {
		s := openClosedStore(t)
		wantCredentialFree(t, s.Delete(ctx, markerSID), "session: delete: ", markerSID)
	})
	t.Run("bump", func(t *testing.T) {
		s := openClosedStore(t)
		wantCredentialFree(t, s.BumpTTL(ctx, markerSID, time.Now().Add(time.Hour)), "session: bump: ", markerSID)
	})
	t.Run("listUser", func(t *testing.T) {
		s := openClosedStore(t)
		_, err := s.ListByUser(ctx, markerUserID)
		wantCredentialFree(t, err, "session: list user: ", markerUserID)
	})
	t.Run("revokeUser", func(t *testing.T) {
		s := openClosedStore(t)
		_, err := s.RevokeByUser(ctx, markerUserID)
		wantCredentialFree(t, err, "session: revoke user: ", markerUserID)
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

// wantCredentialFree pins both the credential-free prefix and the
// absence of the marker anywhere in the error text - a prefix match
// alone would pass "session: load: MARKER: ...".
func wantCredentialFree(t *testing.T, err error, prefix, marker string) {
	t.Helper()
	wantPrefix(t, err, prefix)
	if strings.Contains(err.Error(), marker) {
		t.Fatalf("error carries the marker identifier: %v", err)
	}
}
