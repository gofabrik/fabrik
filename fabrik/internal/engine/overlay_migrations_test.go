package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWireMigrationOverlay covers migration validation through editor overlays.
func TestWireMigrationOverlay(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	migrationsDir, err := filepath.Abs("../../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	write := func(rel, content string) {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module app\n\ngo 1.26\n\nrequire github.com/gofabrik/fabrik/migrations v0.0.0\n\nreplace github.com/gofabrik/fabrik/migrations => "+migrationsDir+"\n")
	write("main.go", "package main\n\nfunc main() { _ = run }\n")
	write("shared/migrations.go", "package shared\n\nimport \"embed\"\n\n//fabrik:migrations\n//go:embed all:migrations\nvar Migrations embed.FS\n")
	write("shared/migrations/0001_users.sql", "CREATE TABLE users (id INTEGER PRIMARY KEY);\n")

	res, err := Wire(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Diags.HasFatal() {
		t.Fatalf("clean tree should wire: %v", res.Diags)
	}

	wantDiag := func(t *testing.T, overlay map[string][]byte, needle string) {
		t.Helper()
		res, err := Wire(dir, overlay)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range res.Diags {
			if strings.Contains(d.Message, needle) {
				return
			}
		}
		t.Fatalf("overlay migration error %q not surfaced: %v", needle, res.Diags)
	}

	wantDiag(t, map[string][]byte{
		filepath.Join(dir, "shared/migrations/not-a-migration.sql"): []byte("SELECT 1;\n"),
	}, "not-a-migration.sql")

	wantDiag(t, map[string][]byte{
		filepath.Join(dir, "shared/migrations/0001_dupe.sql"): []byte("SELECT 1;\n"),
	}, "version 1")
}
