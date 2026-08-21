package postgres_test

import (
	"testing"

	"github.com/gofabrik/fabrik/auth/store/postgres"
)

// SchemaStatements preserves the shipped DDL and statement order.
func TestSchemaUnchanged(t *testing.T) {
	want := []string{
		`CREATE TABLE IF NOT EXISTS identities (
    id         BYTEA  PRIMARY KEY,
    status     TEXT   NOT NULL,
    claims     TEXT   NOT NULL,
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL
);`,
		`CREATE TABLE IF NOT EXISTS password_credentials (
    identity_id BYTEA  NOT NULL UNIQUE REFERENCES identities(id) ON DELETE CASCADE,
    email       BYTEA  NOT NULL UNIQUE,
    hash        BYTEA  NOT NULL,
    created_at  BIGINT NOT NULL,
    updated_at  BIGINT NOT NULL
);`,
	}
	stmts := postgres.SchemaStatements()
	if len(stmts) != len(want) {
		t.Fatalf("SchemaStatements() returned %d statements, want %d", len(stmts), len(want))
	}
	for i := range want {
		if stmts[i] != want[i] {
			t.Fatalf("statement %d = %q, want the shipped DDL", i, stmts[i])
		}
	}
	if got := postgres.Schema(); got != want[0]+"\n"+want[1] {
		t.Fatalf("Schema() = %q, want the joined statements", got)
	}
}

func TestNewRejectsNilDB(t *testing.T) {
	if _, err := postgres.New(nil, postgres.Options{}); err == nil {
		t.Fatal("nil db accepted")
	}
}
