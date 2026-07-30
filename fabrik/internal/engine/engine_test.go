package engine

import (
	"bytes"
	"flag"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/diag"
	"golang.org/x/tools/txtar"
)

var update = flag.Bool("update", false, "rewrite the want/ sections of testdata fixtures")

// TestFixtures verifies golden fixtures and deterministic generation.
func TestFixtures(t *testing.T) {
	files, err := filepath.Glob("testdata/*.txt")
	if err != nil || len(files) == 0 {
		t.Fatalf("no testdata fixtures: %v", err)
	}
	for _, fixture := range files {
		t.Run(strings.TrimSuffix(filepath.Base(fixture), ".txt"), func(t *testing.T) {
			t.Parallel() // fixtures share nothing; each Wire is a fresh registry
			runFixture(t, fixture)
		})
	}
}

func TestGuardedScopePassConvertsPanicToError(t *testing.T) {
	ds, err := guardedScopePass("materialization", func() diag.Diagnostics {
		panic("boom")
	})
	if err == nil || !strings.Contains(err.Error(), "command scope materialization panicked: boom") {
		t.Fatalf("guardedScopePass error = %v", err)
	}
	if ds != nil {
		t.Fatalf("guardedScopePass diagnostics = %v, want nil", ds)
	}
}

func TestCompileFixtureReportsBuildOutput(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped under -short")
	}

	dir := t.TempDir()
	gomod := "module fixture\n\ngo 1.26\n\nreplace (\n\tgithub.com/gofabrik/fabrik/config => /tmp/x\n)\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o600); err != nil {
		t.Fatal(err)
	}
	replaces := map[string]string{
		"github.com/gofabrik/fabrik/config": "/elsewhere",
		"github.com/gofabrik/fabrik/router": "/local/router",
	}
	err := compileFixture(dir, dir, []byte("package main\n\nfunc main() { missing() }\n"), replaces)
	if err == nil || !strings.Contains(err.Error(), "go build failed") || !strings.Contains(err.Error(), "undefined: missing") {
		t.Fatalf("compileFixture error = %v", err)
	}
	mod, err := os.ReadFile(filepath.Join(dir, "go.mod")) // #nosec G304 -- reads back the test-written go.mod
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(mod, []byte("replace github.com/gofabrik/fabrik/router => /local/router")) {
		t.Errorf("missing appended replace for router:\n%s", mod)
	}
	if bytes.Count(mod, []byte("github.com/gofabrik/fabrik/config =>")) != 1 {
		t.Errorf("block-form replace duplicated:\n%s", mod)
	}
}

// compileFixture builds the generated source inside the fixture module.
// Missing replaces for fabrik modules are appended first so the build
// resolves the working tree, never a published module.
func compileFixture(dir, mainDir string, src []byte, replaces map[string]string) error {
	if err := os.WriteFile(filepath.Join(mainDir, "main.gen.go"), src, 0o600); err != nil {
		return err
	}
	modPath := filepath.Join(dir, "go.mod")
	mod, err := os.ReadFile(modPath) // #nosec G304 -- fixture temp dir
	if err != nil {
		return err
	}
	var extra []byte
	for _, path := range slices.Sorted(maps.Keys(replaces)) {
		if !bytes.Contains(mod, []byte(path+" =>")) {
			extra = append(extra, []byte("\nreplace "+path+" => "+replaces[path]+"\n")...)
		}
	}
	if len(extra) > 0 {
		// #nosec G703 -- fixture temp module path
		if err := os.WriteFile(modPath, append(mod, extra...), 0o600); err != nil {
			return err
		}
	}
	// #nosec G204 -- the command and all arguments are controlled by this test
	build := exec.Command("go", "build", "-o", os.DevNull, ".")
	build.Dir = mainDir
	build.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
	if b, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("go build failed: %w\n%s\n--- generated ---\n%s", err, b, src)
	}
	return nil
}

func runFixture(t *testing.T, fixture string) {
	data, err := os.ReadFile(fixture) // #nosec G304 -- reads a test-selected fixture path
	if err != nil {
		t.Fatal(err)
	}
	ar := txtar.Parse(data)

	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	routerDir, err := filepath.Abs("../../../router")
	if err != nil {
		t.Fatal(err)
	}
	configDir, err := filepath.Abs("../../../config")
	if err != nil {
		t.Fatal(err)
	}
	templateDir, err := filepath.Abs("../../../templates")
	if err != nil {
		t.Fatal(err)
	}
	webDir, err := filepath.Abs("../../../web")
	if err != nil {
		t.Fatal(err)
	}
	assetsDir, err := filepath.Abs("../../../assetmapper")
	if err != nil {
		t.Fatal(err)
	}
	migrationsDir, err := filepath.Abs("../../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	jobsDir, err := filepath.Abs("../../../jobs")
	if err != nil {
		t.Fatal(err)
	}
	cliDir, err := filepath.Abs("../../../cli")
	if err != nil {
		t.Fatal(err)
	}
	httpserverDir, err := filepath.Abs("../../../httpserver")
	if err != nil {
		t.Fatal(err)
	}
	var wantGen, wantDiags []byte
	hasGen, hasDiags := false, false
	for _, f := range ar.Files {
		switch f.Name {
		case "want/main.gen.go":
			wantGen, hasGen = f.Data, true
			continue
		case "want/diags":
			wantDiags, hasDiags = f.Data, true
			continue
		}
		path := filepath.Join(dir, f.Name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		// Fixtures resolve local module checkouts; only generated output imports config.
		data := bytes.ReplaceAll(f.Data, []byte("ROUTERDIR"), []byte(routerDir))
		data = bytes.ReplaceAll(data, []byte("CONFIGDIR"), []byte(configDir))
		data = bytes.ReplaceAll(data, []byte("TEMPLATEDIR"), []byte(templateDir))
		data = bytes.ReplaceAll(data, []byte("WEBDIR"), []byte(webDir))
		data = bytes.ReplaceAll(data, []byte("ASSETSDIR"), []byte(assetsDir))
		data = bytes.ReplaceAll(data, []byte("MIGRATIONSDIR"), []byte(migrationsDir))
		data = bytes.ReplaceAll(data, []byte("JOBSDIR"), []byte(jobsDir))
		data = bytes.ReplaceAll(data, []byte("CLIDIR"), []byte(cliDir))
		data = bytes.ReplaceAll(data, []byte("HTTPSERVERDIR"), []byte(httpserverDir))
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	res, err := Wire(dir, nil)
	if err != nil {
		t.Fatalf("Wire: %v", err)
	}
	gotDiags := renderDiags(res.Diags, dir)

	if res.Src != nil {
		again, err := Wire(dir, nil)
		if err != nil {
			t.Fatalf("Wire (second run): %v", err)
		}
		if !bytes.Equal(res.Src, again.Src) {
			t.Errorf("generation is not deterministic:\nfirst:\n%s\nsecond:\n%s", res.Src, again.Src)
		}
		if !testing.Short() {
			replaces := map[string]string{
				"github.com/gofabrik/fabrik/router":      routerDir,
				"github.com/gofabrik/fabrik/config":      configDir,
				"github.com/gofabrik/fabrik/templates":   templateDir,
				"github.com/gofabrik/fabrik/web":         webDir,
				"github.com/gofabrik/fabrik/assetmapper": assetsDir,
				"github.com/gofabrik/fabrik/migrations":  migrationsDir,
				"github.com/gofabrik/fabrik/jobs":        jobsDir,
				"github.com/gofabrik/fabrik/cli":         cliDir,
				"github.com/gofabrik/fabrik/httpserver":  httpserverDir,
			}
			if err := compileFixture(dir, res.MainDir, res.Src, replaces); err != nil {
				t.Error(err)
			}
		}
	}

	if *update {
		if !t.Failed() {
			updateFixture(t, fixture, ar, res.Src, gotDiags)
		}
		return
	}

	if hasGen && !bytes.Equal(res.Src, wantGen) {
		t.Errorf("main.gen.go mismatch\n--- want ---\n%s--- got ---\n%s", wantGen, res.Src)
	}
	if !hasGen && res.Src != nil {
		t.Errorf("unexpected generated output (no want/main.gen.go in fixture):\n%s", res.Src)
	}
	if hasDiags && gotDiags != string(wantDiags) {
		t.Errorf("diagnostics mismatch\n--- want ---\n%s--- got ---\n%s", wantDiags, gotDiags)
	}
	if !hasDiags && gotDiags != "" {
		t.Errorf("unexpected diagnostics:\n%s", gotDiags)
	}
}

// renderDiags formats root-relative fixture diagnostics.
func renderDiags(ds diag.Diagnostics, root string) string {
	scrub := func(s string) string {
		return strings.ReplaceAll(s, root+string(filepath.Separator), "$WORK/")
	}
	var b strings.Builder
	for _, d := range ds {
		sev := "error"
		if d.Severity == diag.SevWarning {
			sev = "warning"
		}
		rel := strings.TrimPrefix(d.Pos.Filename, root+string(filepath.Separator))
		rel = filepath.ToSlash(rel)
		fmt.Fprintf(&b, "%s: %s:%d:%d: %s\n", sev, rel, d.Pos.Line, d.Pos.Column, scrub(d.Message))
		if d.Help != "" {
			fmt.Fprintf(&b, "  help: %s\n", scrub(d.Help))
		}
	}
	return b.String()
}

func updateFixture(t *testing.T, fixture string, ar *txtar.Archive, src []byte, diags string) {
	var kept []txtar.File
	for _, f := range ar.Files {
		if f.Name != "want/main.gen.go" && f.Name != "want/diags" {
			kept = append(kept, f)
		}
	}
	if src != nil {
		kept = append(kept, txtar.File{Name: "want/main.gen.go", Data: src})
	}
	if diags != "" {
		kept = append(kept, txtar.File{Name: "want/diags", Data: []byte(diags)})
	}
	ar.Files = kept
	if err := os.WriteFile(fixture, txtar.Format(ar), 0o644); err != nil { // #nosec G306 G703 -- fixture updates preserve conventional source-tree permissions at a trusted repo path
		t.Fatal(err)
	}
	t.Logf("updated %s", fixture)
}
