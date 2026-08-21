// Package dbtest runs store conformance against driver-backed stores.
package dbtest

import (
	"database/sql"
	"path/filepath"
	"testing"

	jobssqlite "github.com/gofabrik/fabrik/jobs/sqlite"
	"github.com/gofabrik/fabrik/jobs/storetest"

	_ "modernc.org/sqlite"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "jobs.db") +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		// #nosec G104 -- best-effort database cleanup after the test completes
		db.Close() //nolint:errcheck // best-effort database cleanup after the test completes
	})
	return db
}

func TestSQLiteStore_Conformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) storetest.Backend {
		db := openDB(t)
		s, err := jobssqlite.New(db, jobssqlite.Options{AutoCreate: true})
		if err != nil {
			t.Fatal(err)
		}
		return storetest.Backend{Store: s, DB: db}
	})
}
