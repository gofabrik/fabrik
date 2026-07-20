package candidateproxy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/internal/tools/modset"
)

func twoVersionProxy(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write(t, root, "LICENSE", "license\n")
	write(t, root, "lib/go.mod", "module github.com/gofabrik/fabrik/lib\n\ngo 1.26\n")
	write(t, root, "lib/lib.go", "package lib\n\nconst V = \"v0.1.0\"\n")
	git(t, root, "init")
	git(t, root, "add", "-A")
	git(t, root, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "v0.1.0")
	git(t, root, "tag", "lib/v0.1.0")
	write(t, root, "lib/lib.go", "package lib\n\nconst V = \"v0.1.1\"\n")
	git(t, root, "add", "-A")
	git(t, root, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "v0.1.1")
	git(t, root, "tag", "lib/v0.1.1")

	cfg := &modset.Config{
		Root:      root,
		Version:   "v0.1.0",
		Published: map[string]bool{"github.com/gofabrik/fabrik/lib": true},
		Modules:   map[string]string{"github.com/gofabrik/fabrik/lib": filepath.Join(root, "lib")},
	}
	proxy := t.TempDir()
	if err := writeModule(proxy, cfg.Root, "lib/v0.1.0", "github.com/gofabrik/fabrik/lib", "v0.1.0", "lib"); err != nil {
		t.Fatal(err)
	}
	if err := writeModule(proxy, cfg.Root, "lib/v0.1.1", "github.com/gofabrik/fabrik/lib", "v0.1.1", "lib"); err != nil {
		t.Fatal(err)
	}
	list := filepath.Join(proxy, "github.com/gofabrik/fabrik/lib/@v/list")
	if err := os.WriteFile(list, []byte("v0.1.0\nv0.1.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return proxy
}

func TestScaffoldRepinSurvivesNewerRelease(t *testing.T) {
	proxy := twoVersionProxy(t)
	const lib = "github.com/gofabrik/fabrik/lib"

	run := func(t *testing.T, repin bool) string {
		app := t.TempDir()
		modcache := t.TempDir()
		t.Cleanup(func() { makeWritable(modcache) })
		env := append(os.Environ(), Env(proxy, modcache)...)

		write(t, app, "go.mod", "module example.com/app\n\ngo 1.26\n\nrequire "+lib+" v0.1.0\n")
		write(t, app, "app.go", "package main\n\nfunc main() {}\n")
		goMod(t, app, env, "mod", "tidy")

		write(t, app, "gen.go", "package main\n\nimport _ \""+lib+"\"\n")
		if repin {
			goMod(t, app, env, "mod", "edit", "-require="+lib+"@v0.1.0")
		}
		goMod(t, app, env, "mod", "tidy")

		// #nosec G304 -- reads a test-controlled temporary path
		data, err := os.ReadFile(filepath.Join(app, "go.mod"))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	if got := run(t, true); !strings.Contains(got, lib+" v0.1.0") {
		t.Fatalf("re-pin should keep v0.1.0, go.mod:\n%s", got)
	}
	if got := run(t, false); !strings.Contains(got, lib+" v0.1.1") {
		t.Fatalf("without re-pin, tidy should drift to v0.1.1, go.mod:\n%s", got)
	}
}

func goMod(t *testing.T, dir string, env []string, args ...string) {
	t.Helper()
	cmd := exec.Command("go", args...) // #nosec G204 -- test launches the go toolchain with controlled args
	cmd.Dir = dir
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
