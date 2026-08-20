package postgres_test

import (
	"testing"

	"github.com/gofabrik/fabrik/cache/postgres"
)

// TestSchemaUnchanged preserves compatibility with deployed schemas.
func TestSchemaUnchanged(t *testing.T) {
	const want = `CREATE TABLE IF NOT EXISTS cache_entries (
    key        BYTEA PRIMARY KEY,
    value      BYTEA NOT NULL,
    expires_at BIGINT
);
CREATE INDEX IF NOT EXISTS cache_entries_expires_at ON cache_entries(expires_at);`
	if got := postgres.Schema(); got != want {
		t.Fatalf("Schema() = %q, want the shipped DDL", got)
	}
}

func TestNewRejectsNilDB(t *testing.T) {
	if _, err := postgres.New(nil, postgres.Options{}); err == nil {
		t.Fatal("nil db accepted")
	}
}
