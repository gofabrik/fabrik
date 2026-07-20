package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/internal/tools/modset"
)

type modSpec struct {
	dir      string
	path     string
	requires map[string]string
	imports  []string
}

// fixture creates a published synthetic workspace at version.
func fixture(t *testing.T, version string, mods []modSpec) *modset.Config {
	t.Helper()
	root := t.TempDir()

	var published, uses strings.Builder
	for _, m := range mods {
		fmt.Fprintf(&published, "      - %s\n", m.path)
		fmt.Fprintf(&uses, "\t./%s\n", m.dir)
	}
	versions := "module-sets:\n  fabrik:\n    version: " + version + "\n    modules:\n" + published.String()
	write(t, filepath.Join(root, "versions.yaml"), versions)
	write(t, filepath.Join(root, "go.work"), "go 1.26\n\nuse (\n"+uses.String()+")\n")

	for _, m := range mods {
		dir := filepath.Join(root, m.dir)
		gomod := "module " + m.path + "\n\ngo 1.26\n"
		if len(m.requires) > 0 {
			gomod += "\nrequire (\n"
			for p, v := range m.requires {
				gomod += "\t" + p + " " + v + "\n"
			}
			gomod += ")\n"
		}
		write(t, filepath.Join(dir, "go.mod"), gomod)

		src := "package " + pkgName(m.path) + "\n"
		for i, imp := range m.imports {
			src += fmt.Sprintf("import _%d %q\n", i, imp)
		}
		write(t, filepath.Join(dir, "code.go"), src)
	}

	cfg, err := modset.Load(root)
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	return cfg
}

func pkgName(modPath string) string {
	base := modPath[strings.LastIndex(modPath, "/")+1:]
	return strings.NewReplacer(".", "", "-", "").Replace(base)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func kinds(findings []Finding) []string {
	var out []string
	for _, f := range findings {
		out = append(out, f.Kind)
	}
	return out
}

func TestAnalyzeConsistent(t *testing.T) {
	cfg := fixture(t, "v0.1.0", []modSpec{
		{dir: "b", path: "example.com/b"},
		{
			dir: "a", path: "example.com/a",
			requires: map[string]string{"example.com/b": "v0.1.0"},
			imports:  []string{"example.com/b"},
		},
	})
	findings, err := Analyze(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("want no findings, got %v", findings)
	}
}

func TestAnalyzeMissingRequire(t *testing.T) {
	cfg := fixture(t, "v0.1.0", []modSpec{
		{dir: "b", path: "example.com/b"},
		{dir: "a", path: "example.com/a", imports: []string{"example.com/b"}},
	})
	findings, err := Analyze(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(findings); len(got) != 1 || got[0] != "missing-require" {
		t.Fatalf("want one missing-require, got %v", findings)
	}
}

func TestAnalyzeWrongVersion(t *testing.T) {
	cfg := fixture(t, "v0.1.0", []modSpec{
		{dir: "b", path: "example.com/b"},
		{
			dir: "a", path: "example.com/a",
			requires: map[string]string{"example.com/b": "v0.0.9"},
			imports:  []string{"example.com/b"},
		},
	})
	findings, err := Analyze(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(findings); len(got) != 1 || got[0] != "wrong-version" {
		t.Fatalf("want one wrong-version, got %v", findings)
	}
}

func TestAnalyzeCycle(t *testing.T) {
	cfg := fixture(t, "v0.1.0", []modSpec{
		{
			dir: "a", path: "example.com/a",
			requires: map[string]string{"example.com/b": "v0.1.0"},
			imports:  []string{"example.com/b"},
		},
		{
			dir: "b", path: "example.com/b",
			requires: map[string]string{"example.com/a": "v0.1.0"},
			imports:  []string{"example.com/a"},
		},
	})
	findings, err := Analyze(cfg)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range findings {
		if f.Kind == "cycle" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want a cycle finding, got %v", findings)
	}
}

func TestFixAddsNewEdge(t *testing.T) {
	cfg := fixture(t, "v0.1.0", []modSpec{
		{dir: "b", path: "example.com/b"},
		{dir: "a", path: "example.com/a", imports: []string{"example.com/b"}},
	})
	changed, err := Fix(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed[0] != "example.com/a" {
		t.Fatalf("want example.com/a changed, got %v", changed)
	}
	gomod, err := os.ReadFile(filepath.Join(cfg.Modules["example.com/a"], "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gomod), "example.com/b v0.1.0") {
		t.Fatalf("go.mod missing pinned require:\n%s", gomod)
	}
	cfg2, err := modset.Load(cfg.Root)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := Analyze(cfg2)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("want clean after fix, got %v", findings)
	}
}

func TestFixRefusesCycle(t *testing.T) {
	cfg := fixture(t, "v0.1.0", []modSpec{
		{
			dir:      "a",
			path:     "example.com/a",
			requires: map[string]string{"example.com/b": "v0.1.0"},
			imports:  []string{"example.com/b"},
		},
		{
			dir:      "b",
			path:     "example.com/b",
			requires: map[string]string{"example.com/a": "v0.1.0"},
			imports:  []string{"example.com/a"},
		},
	})
	if _, err := Fix(cfg); err == nil {
		t.Fatal("Fix must refuse a cyclic graph")
	}
}

// Fix must reject cycles introduced only by missing requirements.
func TestFixRefusesImportInducedCycle(t *testing.T) {
	cfg := fixture(t, "v0.1.0", []modSpec{
		{dir: "a", path: "example.com/a", imports: []string{"example.com/b"}},
		{dir: "b", path: "example.com/b", imports: []string{"example.com/a"}},
	})
	if _, err := Fix(cfg); err == nil {
		t.Fatal("Fix must reject an import-induced cycle")
	}
	for _, m := range []string{"example.com/a", "example.com/b"} {
		data, err := os.ReadFile(filepath.Join(cfg.Modules[m], "go.mod"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "require") {
			t.Errorf("%s go.mod was modified despite the cycle:\n%s", m, data)
		}
	}
}
