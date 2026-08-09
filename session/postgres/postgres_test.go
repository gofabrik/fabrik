package postgres_test

import (
	"testing"

	"github.com/gofabrik/fabrik/session/postgres"
)

// TestSchemaUnchanged preserves compatibility with deployed schemas.
func TestSchemaUnchanged(t *testing.T) {
	const want = `CREATE TABLE IF NOT EXISTS sessions (
    sid             BYTEA   PRIMARY KEY,
    version         BIGINT  NOT NULL,
    user_id         BYTEA   NOT NULL DEFAULT '',
    absolute_expiry BIGINT  NOT NULL,
    idle_expiry     BIGINT  NOT NULL,
    payload         BYTEA   NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_user_id ON sessions(user_id) WHERE user_id <> '';
CREATE INDEX IF NOT EXISTS sessions_idle_expiry ON sessions(idle_expiry);`
	if got := postgres.Schema(); got != want {
		t.Fatalf("Schema() = %q, want the shipped DDL", got)
	}
}

func TestNewRejectsNilDB(t *testing.T) {
	if _, err := postgres.New(nil, postgres.Options{}); err == nil {
		t.Fatal("nil db accepted")
	}
}
