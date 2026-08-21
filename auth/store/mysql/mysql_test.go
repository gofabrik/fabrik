package mysql_test

import (
	"testing"

	"github.com/gofabrik/fabrik/auth/store/mysql"
)

// SchemaStatements preserves the shipped DDL and statement order.
func TestSchemaUnchanged(t *testing.T) {
	want := []string{
		"CREATE TABLE IF NOT EXISTS identities (\n" +
			"    id         VARBINARY(191) NOT NULL,\n" +
			"    status     VARCHAR(16)    NOT NULL,\n" +
			"    claims     LONGTEXT       NOT NULL,\n" +
			"    created_at BIGINT         NOT NULL,\n" +
			"    updated_at BIGINT         NOT NULL,\n" +
			"    PRIMARY KEY (id)\n" +
			");",
		"CREATE TABLE IF NOT EXISTS password_credentials (\n" +
			"    identity_id VARBINARY(191)  NOT NULL,\n" +
			"    email       VARBINARY(254)  NOT NULL,\n" +
			"    hash        LONGBLOB        NOT NULL,\n" +
			"    created_at  BIGINT          NOT NULL,\n" +
			"    updated_at  BIGINT          NOT NULL,\n" +
			"    UNIQUE KEY password_credentials_identity_id (identity_id),\n" +
			"    UNIQUE KEY password_credentials_email (email),\n" +
			"    CONSTRAINT password_credentials_identity_fk FOREIGN KEY (identity_id) REFERENCES identities (id) ON DELETE CASCADE\n" +
			");",
	}
	stmts := mysql.SchemaStatements()
	if len(stmts) != len(want) {
		t.Fatalf("SchemaStatements() returned %d statements, want %d", len(stmts), len(want))
	}
	for i := range want {
		if stmts[i] != want[i] {
			t.Fatalf("statement %d = %q, want the shipped DDL", i, stmts[i])
		}
	}
	if got := mysql.Schema(); got != want[0]+"\n"+want[1] {
		t.Fatalf("Schema() = %q, want the joined statements", got)
	}
}

func TestNewRejectsNilDB(t *testing.T) {
	if _, err := mysql.New(nil, mysql.Options{}); err == nil {
		t.Fatal("nil db accepted")
	}
}
