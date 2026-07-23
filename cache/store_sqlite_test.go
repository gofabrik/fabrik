package cache_test

import (
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/cache"
)

func TestNewSQLiteStoreRequiresDB(t *testing.T) {
	if _, err := cache.NewSQLiteStore(nil, cache.SQLiteOptions{}); err == nil {
		t.Fatal("nil db accepted")
	}
}

func TestSQLiteSchemaShape(t *testing.T) {
	ddl := cache.SQLiteSchema()
	for _, want := range []string{"CREATE TABLE IF NOT EXISTS cache_entries", "expires_at INTEGER"} {
		if !strings.Contains(ddl, want) {
			t.Fatalf("schema missing %q:\n%s", want, ddl)
		}
	}
	if strings.Contains(ddl, "expires_at INTEGER NOT NULL") {
		t.Fatal("expires_at must be nullable: NULL is the no-expiry sentinel")
	}
}
