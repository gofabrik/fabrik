package gittag

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/internal/tools/modset"
)

func TestPlan(t *testing.T) {
	cfg := &modset.Config{
		Root:    "/repo",
		Version: "v0.1.0",
		Published: map[string]bool{
			"github.com/gofabrik/fabrik/jobs":           true,
			"github.com/gofabrik/fabrik/jobs/directive": true,
			"github.com/gofabrik/fabrik/fabrik":         true,
		},
		Modules: map[string]string{
			"github.com/gofabrik/fabrik/jobs":           "/repo/jobs",
			"github.com/gofabrik/fabrik/jobs/directive": "/repo/jobs/directive",
			"github.com/gofabrik/fabrik/fabrik":         "/repo/fabrik",
		},
	}
	tags, err := Plan(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"fabrik/v0.1.0", "jobs/directive/v0.1.0", "jobs/v0.1.0"}
	if !reflect.DeepEqual(tags, want) {
		t.Fatalf("Plan = %v, want %v", tags, want)
	}
}

func TestCreateIdempotentAndVersionGuard(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "versions.yaml"),
		"module-sets:\n  fabrik:\n    version: v0.1.0\n")
	writeFile(t, filepath.Join(root, "jobs", "go.mod"), "module x\n")
	mustGit(t, root, "init")
	// Persist an identity in the repo: Create runs `git tag -a`, which needs a
	// committer/tagger and would otherwise fail on a runner with no global git
	// config.
	mustGit(t, root, "config", "user.email", "t@t")
	mustGit(t, root, "config", "user.name", "t")
	mustGit(t, root, "add", "-A")
	mustGit(t, root, "commit", "-m", "init")
	head, _ := run(root, "rev-parse", "HEAD")

	cfg := &modset.Config{
		Root: root, Version: "v0.1.0",
		Published: map[string]bool{"github.com/gofabrik/fabrik/jobs": true},
		Modules:   map[string]string{"github.com/gofabrik/fabrik/jobs": filepath.Join(root, "jobs")},
	}

	if _, err := Create(cfg, head, "origin", false); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if sha, ok := tagCommit(root, "jobs/v0.1.0"); !ok || sha != head {
		t.Fatalf("tag not at HEAD: %s ok=%v", sha, ok)
	}
	if _, err := Create(cfg, head, "origin", false); err != nil {
		t.Fatalf("re-run should be idempotent: %v", err)
	}
	cfg.Version = "v0.2.0"
	if _, err := Create(cfg, head, "origin", false); err == nil {
		t.Fatal("expected version-guard error when cfg version != commit's versions.yaml")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func run(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...) // #nosec G204 -- test launches git with controlled args
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if _, err := run(dir, args...); err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
}
