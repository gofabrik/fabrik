package mysql_test

import (
	"testing"

	"github.com/gofabrik/fabrik/session/mysql"
)

// TestSchemaUnchanged preserves compatibility with deployed schemas.
func TestSchemaUnchanged(t *testing.T) {
	const want = "CREATE TABLE IF NOT EXISTS sessions (\n" +
		"    sid             VARBINARY(3072) NOT NULL,\n" +
		"    version         BIGINT          NOT NULL,\n" +
		"    user_id         VARBINARY(191)  NOT NULL DEFAULT '',\n" +
		"    absolute_expiry BIGINT          NOT NULL,\n" +
		"    idle_expiry     BIGINT          NOT NULL,\n" +
		"    payload         LONGBLOB        NOT NULL,\n" +
		"    PRIMARY KEY (sid),\n" +
		"    INDEX sessions_user_id (user_id),\n" +
		"    INDEX sessions_idle_expiry (idle_expiry)\n" +
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
