package candidateproxy

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/sumdb/dirhash"

	"github.com/gofabrik/fabrik/internal/tools/modset"
)

// gitFixture includes both root-LICENSE inheritance and a nested module boundary.
func gitFixture(t *testing.T) *modset.Config {
	t.Helper()
	root := t.TempDir()
	write(t, root, "LICENSE", "Copyright. Permission is granted.\n")
	write(t, root, "versions.yaml",
		"module-sets:\n  fabrik:\n    version: v1.0.0\n    modules:\n"+
			"      - github.com/gofabrik/fabrik/router\n"+
			"      - github.com/gofabrik/fabrik/router/directive\n")
	write(t, root, "go.work", "go 1.26\n\nuse (\n\t./router\n\t./router/directive\n)\n")
	write(t, root, "router/go.mod", "module github.com/gofabrik/fabrik/router\n\ngo 1.26\n")
	write(t, root, "router/r.go", "package router\n\nfunc R() {}\n")
	write(t, root, "router/directive/go.mod", "module github.com/gofabrik/fabrik/router/directive\n\ngo 1.26\n")
	write(t, root, "router/directive/d.go", "package directive\n\nfunc D() {}\n")

	git(t, root, "init")
	git(t, root, "add", "-A")
	git(t, root, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "init")

	cfg, err := modset.Load(root)
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	return cfg
}

func TestBuildCanonicalArchive(t *testing.T) {
	cfg := gitFixture(t)
	out := t.TempDir()
	if err := Build(cfg, out, "HEAD"); err != nil {
		t.Fatalf("build: %v", err)
	}

	// #nosec G304 -- reads a test-controlled temporary path
	data, err := os.ReadFile(filepath.Join(out,
		"github.com/gofabrik/fabrik/router/@v/v1.0.0.zip"))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := zipEntries(data)
	if err != nil {
		t.Fatal(err)
	}
	const prefix = "github.com/gofabrik/fabrik/router@v1.0.0/"
	has := func(name string) bool {
		for _, e := range entries {
			if e == prefix+name {
				return true
			}
		}
		return false
	}
	if !has("r.go") {
		t.Errorf("module source missing from zip: %v", entries)
	}
	if !has("LICENSE") {
		t.Errorf("root LICENSE not carried into subdirectory module: %v", entries)
	}
	for _, e := range entries {
		if strings.Contains(e, "directive") {
			t.Errorf("nested module leaked into parent zip: %s", e)
		}
	}
}

func TestArchiveHashConsumable(t *testing.T) {
	cfg := gitFixture(t)
	out := t.TempDir()
	if err := Build(cfg, out, "HEAD"); err != nil {
		t.Fatalf("build: %v", err)
	}
	zipPath := filepath.Join(out, "github.com/gofabrik/fabrik/router/@v/v1.0.0.zip")
	ourHash, err := dirhash.HashZip(zipPath, dirhash.Hash1)
	if err != nil {
		t.Fatal(err)
	}

	consumer := t.TempDir()
	write(t, consumer, "go.mod", "module example.com/consumer\n\ngo 1.26\n")
	modcache := t.TempDir()
	// Go's read-only cache must be writable before temporary-directory cleanup.
	t.Cleanup(func() { makeWritable(modcache) })

	dl := exec.Command("go", "mod", "download", "-json", "github.com/gofabrik/fabrik/router@v1.0.0")
	dl.Dir = consumer
	dl.Env = append(os.Environ(), Env(out, modcache)...)
	o, err := dl.CombinedOutput()
	if err != nil {
		t.Fatalf("go mod download: %v\n%s", err, o)
	}
	var res struct{ Sum string }
	if err := json.Unmarshal(o, &res); err != nil {
		t.Fatalf("parse download json: %v\n%s", err, o)
	}
	if res.Sum != ourHash {
		t.Fatalf("hash mismatch: proxy zip %s, go recorded %s", ourHash, res.Sum)
	}
}

func TestHashMatchesGoVCSDownload(t *testing.T) {
	cfg := gitFixture(t)
	git(t, cfg.Root, "tag", "router/v1.0.0")
	git(t, cfg.Root, "tag", "router/directive/v1.0.0")

	out := t.TempDir()
	if err := Build(cfg, out, "HEAD"); err != nil {
		t.Fatalf("build candidate: %v", err)
	}

	bare := filepath.Join(t.TempDir(), "bare.git")
	// #nosec G204 -- launches the fixed git binary with controlled test paths
	if o, err := exec.Command("git", "clone", "--bare", "-q", cfg.Root, bare).CombinedOutput(); err != nil {
		t.Fatalf("bare clone: %v\n%s", err, o)
	}
	gitcfg := filepath.Join(t.TempDir(), "gitconfig")
	write(t, filepath.Dir(gitcfg), "gitconfig",
		"[url \"file://"+bare+"\"]\n\tinsteadOf = https://github.com/gofabrik/fabrik\n")

	for _, mod := range []string{
		"github.com/gofabrik/fabrik/router",
		"github.com/gofabrik/fabrik/router/directive",
	} {
		candHash, err := dirhash.HashZip(
			filepath.Join(out, filepath.FromSlash(mod), "@v", "v1.0.0.zip"), dirhash.Hash1)
		if err != nil {
			t.Fatalf("%s candidate hash: %v", mod, err)
		}
		goSum := goVCSSum(t, gitcfg, mod+"@v1.0.0")
		if goSum != candHash {
			t.Errorf("%s: go-from-VCS hash %s != candidate hash %s", mod, goSum, candHash)
		}
	}
}

func goVCSSum(t *testing.T, gitcfg, modver string) string {
	t.Helper()
	consumer := t.TempDir()
	write(t, consumer, "go.mod", "module example.com/c\n\ngo 1.26\n")
	modcache := t.TempDir()
	t.Cleanup(func() { makeWritable(modcache) })

	cmd := exec.Command("go", "mod", "download", "-json", modver) // #nosec G204 -- test launches the go toolchain with controlled args
	cmd.Dir = consumer
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+gitcfg,
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GOPRIVATE=github.com/gofabrik/fabrik/*",
		"GOPROXY=direct",
		"GOVCS=*:all",
		"GOFLAGS=-mod=mod",
		"GOWORK=off",
		"GOMODCACHE="+modcache,
	)
	o, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go mod download %s: %v\n%s", modver, err, o)
	}
	var res struct{ Sum string }
	if err := json.Unmarshal(o, &res); err != nil {
		t.Fatalf("parse download json: %v\n%s", err, o)
	}
	return res.Sum
}

func TestWorktreeMatchesVCS(t *testing.T) {
	cfg := gitFixture(t)

	vcsOut := t.TempDir()
	if err := Build(cfg, vcsOut, "HEAD"); err != nil {
		t.Fatal(err)
	}
	wtOut := t.TempDir()
	if err := BuildWorktree(cfg, wtOut); err != nil {
		t.Fatal(err)
	}

	for path := range cfg.Published {
		enc := filepath.FromSlash(path)
		rel := filepath.Join(enc, "@v", "v1.0.0.zip")
		vcsHash, err := dirhash.HashZip(filepath.Join(vcsOut, rel), dirhash.Hash1)
		if err != nil {
			t.Fatalf("%s vcs hash: %v", path, err)
		}
		wtHash, err := dirhash.HashZip(filepath.Join(wtOut, rel), dirhash.Hash1)
		if err != nil {
			t.Fatalf("%s worktree hash: %v", path, err)
		}
		if vcsHash != wtHash {
			t.Errorf("%s: worktree hash %s != vcs hash %s", path, wtHash, vcsHash)
		}
	}
}

func TestBuildEscapesUppercaseVersion(t *testing.T) {
	cfg := gitFixture(t)
	cfg.Version = "v1.0.0-RC1"
	out := t.TempDir()
	if err := BuildWorktree(cfg, out); err != nil {
		t.Fatalf("build: %v", err)
	}
	vdir := filepath.Join(out, "github.com/gofabrik/fabrik/router/@v")
	if _, err := os.Stat(filepath.Join(vdir, "v1.0.0-!r!c1.zip")); err != nil {
		t.Fatalf("escaped zip missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(vdir, "v1.0.0-RC1.zip")); !os.IsNotExist(err) {
		t.Fatalf("raw (unescaped) zip should not exist")
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

func makeWritable(root string) {
	// #nosec G104 -- best-effort cleanup of a temp cache
	//nolint:errcheck // best-effort cleanup of a temp cache
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil {
			mode := os.FileMode(0o600)
			if d.IsDir() {
				mode = 0o700
			}
			// #nosec G104 -- best-effort cleanup of a temp cache
			//nolint:errcheck // best-effort cleanup of a temp cache
			os.Chmod(p, mode) // #nosec G122 -- trusted test path
		}
		return nil
	})
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...) // #nosec G204 -- test launches the go toolchain with controlled args
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
