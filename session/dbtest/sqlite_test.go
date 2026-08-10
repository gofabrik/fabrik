// Package dbtest runs store conformance against driver-backed stores.
package dbtest

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/gofabrik/fabrik/session"
	sessionsqlite "github.com/gofabrik/fabrik/session/sqlite"
	"github.com/gofabrik/fabrik/session/storetest"

	_ "modernc.org/sqlite"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "sessions.db")+"?_pragma=busy_timeout(5000)")
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
	storetest.Run(t, func(t *testing.T) session.Store {
		s, err := sessionsqlite.New(openDB(t), sessionsqlite.Options{AutoCreate: true})
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

// The schema supports both AutoCreate and externally managed DDL.
func TestSQLiteSchemaIdempotentAndManaged(t *testing.T) {
	db := openDB(t)
	if _, err := sessionsqlite.New(db, sessionsqlite.Options{AutoCreate: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := sessionsqlite.New(db, sessionsqlite.Options{AutoCreate: true}); err != nil {
		t.Fatalf("second AutoCreate: %v", err)
	}

	managed := openDB(t)
	if _, err := managed.Exec(sessionsqlite.Schema()); err != nil {
		t.Fatal(err)
	}
	s, err := sessionsqlite.New(managed, sessionsqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(t.Context(), "missing"); err == nil {
		t.Fatal("load against managed schema should be ErrNotFound, not a table error")
	}
}
