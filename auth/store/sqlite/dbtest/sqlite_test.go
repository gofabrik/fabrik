// Package dbtest tests the SQLite store implementation.
package dbtest

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/auth/password"
	"github.com/gofabrik/fabrik/auth/store"
	storesqlite "github.com/gofabrik/fabrik/auth/store/sqlite"
	"github.com/gofabrik/fabrik/auth/store/storetest"

	_ "modernc.org/sqlite"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "auth.db") +
		"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// #nosec G104 -- test database Close is cleanup
		db.Close() //nolint:errcheck // test database Close is cleanup
	})
	return db
}

func TestSQLiteStoreConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T, now func() time.Time) store.Store {
		s, err := storesqlite.New(openDB(t), storesqlite.Options{AutoCreate: true, Now: now})
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

func TestSQLiteSchemaIdempotentAndManaged(t *testing.T) {
	db := openDB(t)
	if _, err := storesqlite.New(db, storesqlite.Options{AutoCreate: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := storesqlite.New(db, storesqlite.Options{AutoCreate: true}); err != nil {
		t.Fatalf("second AutoCreate: %v", err)
	}

	managed := openDB(t)
	for _, stmt := range storesqlite.SchemaStatements() {
		if _, err := managed.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	s, err := storesqlite.New(managed, storesqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetIdentity(t.Context(), "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get against managed schema = %v, want ErrNotFound", err)
	}
}

// Foreign-key cascades must apply to deletes issued outside Store.
func TestSQLiteForeignKeyCascade(t *testing.T) {
	ctx := t.Context()
	db := openDB(t)
	s, err := storesqlite.New(db, storesqlite.Options{AutoCreate: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateIdentity(ctx, "alice", store.IdentityClaims{}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreatePassword(ctx, "alice", "alice@example.com", "hash"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM identities WHERE id = 'alice'`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM password_credentials`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("credential rows after cascade = %d, want 0", n)
	}
	if _, err := s.Lookup(ctx, "alice@example.com"); !errors.Is(err, password.ErrNotFound) {
		t.Fatalf("Lookup after cascade = %v, want password.ErrNotFound", err)
	}
}
