// Closed database handles fail before dialing.
package dbtest

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/auth/store"
	storepostgres "github.com/gofabrik/fabrik/auth/store/postgres"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func openClosedStore(t *testing.T) *storepostgres.Store {
	t.Helper()
	db, err := sql.Open("pgx", "postgres://localhost:1/none")
	if err != nil {
		t.Fatal(err)
	}
	s, err := storepostgres.New(db, storepostgres.Options{})
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	return s
}

func TestClosedDB_ErrorWraps(t *testing.T) {
	ctx := context.Background()

	t.Run("createSchema", func(t *testing.T) {
		db, err := sql.Open("pgx", "postgres://localhost:1/none")
		if err != nil {
			t.Fatal(err)
		}
		db.Close()
		_, err = storepostgres.New(db, storepostgres.Options{AutoCreate: true})
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
