package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/gofabrik/fabrik/auth/password"
	"github.com/gofabrik/fabrik/auth/store/sqlite"

	_ "modernc.org/sqlite"
)

func newStore(t *testing.T) *sqlite.Store {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "u.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s, err := sqlite.New(db, sqlite.Options{AutoCreate: true})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCreateAndLookup(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u, err := s.Create(ctx, "Alice@Example.com", []byte("hash"))
	if err != nil {
		t.Fatal(err)
	}
	if u.ID == "" || u.Email != "alice@example.com" {
		t.Fatalf("created = %+v (ID non-empty, email normalized)", u)
	}
	// Lookup normalizes too: whitespace + case variant finds it.
	got, err := s.LookupByEmail(ctx, "  ALICE@example.com ")
	if err != nil || got.ID != u.ID {
		t.Fatalf("lookup = %+v, %v", got, err)
	}
}

func TestLookupMissing(t *testing.T) {
	s := newStore(t)
	if _, err := s.LookupByEmail(context.Background(), "nobody@example.com"); !errors.Is(err, password.ErrUserNotFound) {
		t.Fatalf("missing lookup = %v, want ErrUserNotFound", err)
	}
}

func TestCreateDuplicate(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.Create(ctx, "a@example.com", []byte("h")); err != nil {
		t.Fatal(err)
	}
	// Case variant collides via the UNIQUE constraint on normalized email.
	if _, err := s.Create(ctx, "A@Example.com", []byte("h")); !errors.Is(err, password.ErrEmailTaken) {
		t.Fatalf("duplicate = %v, want ErrEmailTaken", err)
	}
}

func TestNewIdempotent(t *testing.T) {
	db, _ := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "u.db"))
	defer db.Close()
	if _, err := sqlite.New(db, sqlite.Options{AutoCreate: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.New(db, sqlite.Options{AutoCreate: true}); err != nil {
		t.Fatalf("second New (schema re-apply): %v", err)
	}
}
