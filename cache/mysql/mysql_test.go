package mysql_test

import (
	"testing"

	"github.com/gofabrik/fabrik/cache/mysql"
)

// TestSchemaUnchanged preserves compatibility with deployed schemas.
func TestSchemaUnchanged(t *testing.T) {
	const want = "CREATE TABLE IF NOT EXISTS cache_entries (\n" +
		"    `key`      VARBINARY(3072) NOT NULL,\n" +
		"    value      LONGBLOB NOT NULL,\n" +
		"    expires_at BIGINT,\n" +
		"    PRIMARY KEY (`key`),\n" +
		"    INDEX cache_entries_expires_at (expires_at)\n" +
		");"
	if got := mysql.Schema(); got != want {
		t.Fatalf("Schema() = %q, want the shipped DDL", got)
	}
}

func TestNewRejectsNilDB(t *testing.T) {
	if _, err := mysql.New(nil, mysql.Options{}); err == nil {
		t.Fatal("nil db accepted")
	}
}
