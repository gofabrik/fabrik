package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"
)

// Pins must survive the full scaffold flow when the proxy serves a newer release.
func TestScaffoldPinsSurviveRealFlow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping scaffold integration test in -short mode")
	}
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	libs := map[string]string{}
	for _, m := range []string{"router", "web", "templates", "config", "assetmapper", "cli", "httpserver"} {
		libs["github.com/gofabrik/fabrik/"+m] = filepath.Join(repoRoot, m)
	}
	proxy := buildScaffoldProxy(t, libs, []string{"v0.1.0", "v0.1.1"})

	orig := scaffoldVersion
	scaffoldVersion = func() string { return "v0.1.0" }
	t.Cleanup(func() { scaffoldVersion = orig })

	modcache := t.TempDir()
	t.Cleanup(func() { chmodTree(modcache, 0o755) })
	t.Setenv("GOWORK", "off")
	t.Setenv("GOPROXY", "file://"+filepath.ToSlash(proxy)+",https://proxy.golang.org")
	t.Setenv("GONOSUMDB", "github.com/gofabrik/fabrik/*")
	t.Setenv("GOMODCACHE", modcache)

	tmp := t.TempDir()
	if r, err := filepath.EvalSymlinks(tmp); err == nil {
		tmp = r
	}
	t.Chdir(tmp)
	if err := newCmd([]string{"hello"}); err != nil {
		t.Fatalf("fabrik new: %v", err)
	}

	gomod, err := os.ReadFile(filepath.Join(tmp, "hello", "go.mod")) // #nosec G304 -- reads a test-controlled temporary path
	if err != nil {
		t.Fatal(err)
	}
	got := string(gomod)
	for path := range libs {
		if !strings.Contains(got, path+" v0.1.0") {
			t.Errorf("expected %s pinned to v0.1.0, go.mod:\n%s", path, got)
		}
		if strings.Contains(got, path+" v0.1.1") {
			t.Errorf("%s drifted to v0.1.1 despite the CLI version being v0.1.0:\n%s", path, got)
		}
	}
}

// buildScaffoldProxy serves identical module sources at each requested version.
func buildScaffoldProxy(t *testing.T, mods map[string]string, versions []string) string {
	t.Helper()
	out := t.TempDir()
	for path, dir := range mods {
		vdir := filepath.Join(out, filepath.FromSlash(path), "@v")
		if err := os.MkdirAll(vdir, 0o750); err != nil {
			t.Fatal(err)
		}
		gomod, err := os.ReadFile(filepath.Join(dir, "go.mod")) // #nosec G304 -- reads a trusted repository module path
		if err != nil {
			t.Fatal(err)
		}
		var list bytes.Buffer
		for _, v := range versions {
			var zbuf bytes.Buffer
			if err := modzip.CreateFromDir(&zbuf, module.Version{Path: path, Version: v}, dir); err != nil {
				t.Fatalf("zip %s@%s: %v", path, v, err)
			}
			writeFile(t, filepath.Join(vdir, v+".zip"), zbuf.Bytes())
			writeFile(t, filepath.Join(vdir, v+".mod"), gomod)
			writeFile(t, filepath.Join(vdir, v+".info"),
				[]byte(fmt.Sprintf(`{"Version":%q,"Time":"2000-01-01T00:00:00Z"}`, v)))
			list.WriteString(v + "\n")
		}
		writeFile(t, filepath.Join(vdir, "list"), list.Bytes())
	}
	return out
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil { // #nosec G703 -- trusted test proxy path
		t.Fatal(err)
	}
}

func chmodTree(root string, mode os.FileMode) {
	//nolint:errcheck // best-effort cleanup of a temporary module cache
	filepath.WalkDir(root, func(p string, _ os.DirEntry, err error) error { // #nosec G104 -- best-effort cleanup of a temporary module cache
		if err == nil {
			//nolint:errcheck // best-effort cleanup of a temporary module cache
			os.Chmod(p, mode) // #nosec G104 -- best-effort cleanup of a temporary module cache
		}
		return nil
	})
}
