package converge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/internal/tools/modset"
)

// fixture creates a three-module chain whose middle archive changes after its first tidy.
func fixture(t *testing.T) *modset.Config {
	t.Helper()
	root := t.TempDir()
	write(t, root, "LICENSE", "license\n")
	write(t, root, "versions.yaml",
		"module-sets:\n  fabrik:\n    version: v0.1.0\n    modules:\n"+
			"      - github.com/gofabrik/fabrik/a\n"+
			"      - github.com/gofabrik/fabrik/b\n"+
			"      - github.com/gofabrik/fabrik/c\n")
	write(t, root, "go.work", "go 1.26\n\nuse (\n\t./a\n\t./b\n\t./c\n)\n")

	write(t, root, "c/go.mod", "module github.com/gofabrik/fabrik/c\n\ngo 1.26\n")
	write(t, root, "c/c.go", "package c\n\nfunc N() string { return \"c\" }\n")
	write(t, root, "b/go.mod",
		"module github.com/gofabrik/fabrik/b\n\ngo 1.26\n\nrequire github.com/gofabrik/fabrik/c v0.1.0\n")
	write(t, root, "b/b.go",
		"package b\n\nimport \"github.com/gofabrik/fabrik/c\"\n\nfunc N() string { return c.N() }\n")
	write(t, root, "a/go.mod",
		"module github.com/gofabrik/fabrik/a\n\ngo 1.26\n\nrequire github.com/gofabrik/fabrik/b v0.1.0\n")
	write(t, root, "a/a.go",
		"package a\n\nimport \"github.com/gofabrik/fabrik/b\"\n\nfunc N() string { return b.N() }\n")

	cfg, err := modset.Load(root)
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	return cfg
}

func TestConvergeAndVerify(t *testing.T) {
	cfg := fixture(t)
	iters, err := Run(cfg, 15)
	if err != nil {
		t.Fatalf("converge: %v", err)
	}
	if iters < 1 {
		t.Fatalf("expected at least one iteration, got %d", iters)
	}
	sum, err := os.ReadFile(filepath.Join(cfg.Modules["github.com/gofabrik/fabrik/a"], "go.sum"))
	if err != nil {
		t.Fatalf("read a go.sum: %v", err)
	}
	if !strings.Contains(string(sum), "github.com/gofabrik/fabrik/b v0.1.0") {
		t.Fatalf("a go.sum missing b entry:\n%s", sum)
	}
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
