package modset

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRejectsPublishedNotInWorkspace(t *testing.T) {
	root := t.TempDir()
	write(t, root, "versions.yaml",
		"module-sets:\n  fabrik:\n    version: v0.1.0\n    modules:\n"+
			"      - example.com/a\n      - example.com/b\n")
	write(t, root, "go.work", "go 1.26\n\nuse (\n\t./a\n)\n")
	write(t, root, "a/go.mod", "module example.com/a\n\ngo 1.26\n")
	write(t, root, "b/go.mod", "module example.com/b\n\ngo 1.26\n")

	_, err := Load(root)
	if err == nil || !strings.Contains(err.Error(), "example.com/b") {
		t.Fatalf("expected an error naming example.com/b, got %v", err)
	}
}

func TestLoadOK(t *testing.T) {
	root := t.TempDir()
	write(t, root, "versions.yaml",
		"module-sets:\n  fabrik:\n    version: v0.1.0\n    modules:\n      - example.com/a\n")
	write(t, root, "go.work", "go 1.26\n\nuse (\n\t./a\n)\n")
	write(t, root, "a/go.mod", "module example.com/a\n\ngo 1.26\n")

	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version != "v0.1.0" {
		t.Fatalf("version = %q", cfg.Version)
	}
	if cfg.Modules["example.com/a"] == "" {
		t.Fatal("module a not resolved")
	}
}
