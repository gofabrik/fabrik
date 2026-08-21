// Closed database handles fail before dialing.
package dbtest

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/auth/store"
	storesqlite "github.com/gofabrik/fabrik/auth/store/sqlite"

	_ "modernc.org/sqlite"
)

func openClosedDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func openClosedStore(t *testing.T) *storesqlite.Store {
	t.Helper()
	db := openClosedDB(t)
	s, err := storesqlite.New(db, storesqlite.Options{})
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
		_, err := storesqlite.New(db, storesqlite.Options{AutoCreate: true})
		wantPrefix(t, err, "store: create schema: ")
	})
	t.Run("createIdentity", func(t *testing.T) {
		s := openClosedStore(t)
		_, err := s.CreateIdentity(ctx, "a", store.IdentityClaims{})
		wantPrefix(t, err, "store: create identity a: ")
	})
	t.Run("getIdentity", func(t *testing.T) {
		s := openClosedStore(t)
		_, err := s.GetIdentity(ctx, "a")
		wantPrefix(t, err, "store: get identity a: ")
	})
	t.Run("setStatus", func(t *testing.T) {
		s := openClosedStore(t)
		wantPrefix(t, s.SetIdentityStatus(ctx, "a", store.StatusDisabled), "store: set status a: ")
	})
	t.Run("setClaims", func(t *testing.T) {
		s := openClosedStore(t)
		wantPrefix(t, s.SetIdentityClaims(ctx, "a", store.IdentityClaims{}), "store: set claims a: ")
	})
	t.Run("deleteIdentity", func(t *testing.T) {
		s := openClosedStore(t)
		wantPrefix(t, s.DeleteIdentity(ctx, "a"), "store: delete identity a: ")
	})
	t.Run("createPassword", func(t *testing.T) {
		s := openClosedStore(t)
		wantPrefix(t, s.CreatePassword(ctx, "a", "a@example.com", "h"), "store: create password a: ")
	})
	t.Run("setPassword", func(t *testing.T) {
		s := openClosedStore(t)
		wantPrefix(t, s.SetPassword(ctx, "a", "a@example.com", "h"), "store: set password a: ")
	})
	t.Run("lookup", func(t *testing.T) {
		s := openClosedStore(t)
		_, err := s.Lookup(ctx, "a@example.com")
		wantPrefix(t, err, "store: lookup: ")
	})
	t.Run("updateHash", func(t *testing.T) {
		s := openClosedStore(t)
		wantPrefix(t, s.UpdateHash(ctx, "a@example.com", "old", "new"), "store: update hash: ")
	})
}

func wantPrefix(t *testing.T, err error, prefix string) {
	t.Helper()
	if err == nil || !strings.HasPrefix(err.Error(), prefix) {
		t.Fatalf("error = %v, want prefix %q", err, prefix)
	}
}
