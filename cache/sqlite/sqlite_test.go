package sqlite_test

import (
	"testing"

	"github.com/gofabrik/fabrik/cache/sqlite"
)

// TestSchemaUnchanged preserves compatibility with the legacy schema.
func TestSchemaUnchanged(t *testing.T) {
	const want = `CREATE TABLE IF NOT EXISTS cache_entries (
    key        TEXT PRIMARY KEY,
    value      BLOB NOT NULL,
    expires_at INTEGER
);
CREATE INDEX IF NOT EXISTS cache_entries_expires_at ON cache_entries(expires_at);`
	if got := sqlite.Schema(); got != want {
		t.Fatalf("Schema() = %q, want the shipped DDL", got)
	}
}

func TestNewRejectsNilDB(t *testing.T) {
	if _, err := sqlite.New(nil, sqlite.Options{}); err == nil {
		t.Fatal("nil db accepted")
	}
}
