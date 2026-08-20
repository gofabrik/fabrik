package sqlite_test

import (
	"testing"

	"github.com/gofabrik/fabrik/session/sqlite"
)

// TestSchemaUnchanged preserves compatibility with the legacy schema.
func TestSchemaUnchanged(t *testing.T) {
	const want = `CREATE TABLE IF NOT EXISTS sessions (
    sid             TEXT    PRIMARY KEY,
    version         INTEGER NOT NULL,
    user_id         TEXT    NOT NULL DEFAULT '',
    absolute_expiry INTEGER NOT NULL,
    idle_expiry     INTEGER NOT NULL,
    payload         BLOB    NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_user_id ON sessions(user_id) WHERE user_id <> '';
CREATE INDEX IF NOT EXISTS sessions_idle_expiry ON sessions(idle_expiry);`
	if got := sqlite.Schema(); got != want {
		t.Fatalf("Schema() = %q, want the shipped DDL", got)
	}
}

func TestNewRejectsNilDB(t *testing.T) {
	if _, err := sqlite.New(nil, sqlite.Options{}); err == nil {
		t.Fatal("nil db accepted")
	}
}
