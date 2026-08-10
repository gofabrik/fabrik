package postgres_test

import (
	"testing"

	"github.com/gofabrik/fabrik/ratelimit/postgres"
)

// TestSchemaUnchanged preserves compatibility with deployed schemas.
func TestSchemaUnchanged(t *testing.T) {
	const want = `CREATE TABLE IF NOT EXISTS ratelimit (
    key        BYTEA   PRIMARY KEY,
    value      BIGINT  NOT NULL,
    expires_at BIGINT  NOT NULL
);
CREATE INDEX IF NOT EXISTS ratelimit_expires_at ON ratelimit(expires_at);`
	if got := postgres.Schema(); got != want {
		t.Fatalf("Schema() = %q, want the shipped DDL", got)
	}
}

func TestNewRejectsNilDB(t *testing.T) {
	if _, err := postgres.New(nil, postgres.Options{}); err == nil {
		t.Fatal("nil db accepted")
	}
}
