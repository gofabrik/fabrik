package goversion

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/internal/tools/modset"
)

// tree describes one fixture repository. Every field defaults to a
// self-consistent tree at goVersion with no toolchain directive; a test
// overrides exactly the field it wants to skew.
type tree struct {
	goVersion    string
	toolchain    string
	modVersions  map[string]string // module dir -> go.mod's go version
	toolsVersion string
	tmplVersion  string
	fixtures     map[string]string // testdata filename -> go.mod section's go version
	workflows    map[string]string // workflow filename -> raw file content
}

func defaultTree() tree {
	return tree{
		goVersion:    "1.26",
		modVersions:  map[string]string{"a": "1.26", "b": "1.26"},
		toolsVersion: "1.26",
		tmplVersion:  "1.26",
		fixtures:     map[string]string{"assets.txt": "1.26"},
		workflows: map[string]string{
			"ci.yml":      workflowContent([]string{"1.26"}),
			"nightly.yml": workflowContent([]string{"1.26", "1.26", "1.26"}),
		},
	}
}

// toolchainTree is a self-consistent tree pinned to an rc toolchain, for tests
// that exercise toolchain-aware workflow matching.
func toolchainTree() tree {
	return tree{
		goVersion:    "1.27",
		toolchain:    "go1.27rc2",
		modVersions:  map[string]string{"a": "1.27", "b": "1.27"},
		toolsVersion: "1.27",
		tmplVersion:  "1.27",
		fixtures:     map[string]string{"assets.txt": "1.27"},
	}
}

// patchedGoTree is a self-consistent tree whose go directive carries a patch
// version, for tests that exercise the major.minor workflow comparison.
func patchedGoTree() tree {
	return tree{
		goVersion:    "1.27.0",
		modVersions:  map[string]string{"a": "1.27.0", "b": "1.27.0"},
		toolsVersion: "1.27.0",
		tmplVersion:  "1.27.0",
		fixtures:     map[string]string{"assets.txt": "1.27.0"},
	}
}

// workflowContent renders one job per pin, each with a single-quoted go-version
// under a setup-go step, matching the repo's actual workflow style.
func workflowContent(pins []string) string {
	var b strings.Builder
	b.WriteString("name: x\non: push\njobs:\n")
	for i, pin := range pins {
		fmt.Fprintf(&b, "  job%d:\n    steps:\n      - uses: actions/setup-go@v7\n        with:\n          go-version: '%s'\n", i, pin)
	}
	return b.String()
}

// stepWorkflow wraps one already-indented step (or steps) in a single job.
func stepWorkflow(step string) string {
	return "name: x\non: push\njobs:\n  job0:\n    steps:\n" + step
}

func build(t *testing.T, tr tree) *modset.Config {
	t.Helper()
	root := t.TempDir()

	var uses, published strings.Builder
	for dir := range tr.modVersions {
		fmt.Fprintf(&uses, "\t./%s\n", dir)
		fmt.Fprintf(&published, "      - example.com/%s\n", dir)
	}
	write(t, root, "versions.yaml",
		"module-sets:\n  fabrik:\n    version: v0.1.0\n    modules:\n"+published.String())

	work := "go " + tr.goVersion + "\n"
	if tr.toolchain != "" {
		work += "toolchain " + tr.toolchain + "\n"
	}
	work += "\nuse (\n" + uses.String() + ")\n"
	write(t, root, "go.work", work)

	for dir, v := range tr.modVersions {
		write(t, root, dir+"/go.mod", "module example.com/"+dir+"\n\ngo "+v+"\n")
	}

	write(t, root, "internal/tools/go.mod",
		"module github.com/gofabrik/fabrik/internal/tools\n\ngo "+tr.toolsVersion+"\n")

	write(t, root, "fabrik/templates/starter/go.mod.tmpl",
		"module {{.Module}}\n\ngo "+tr.tmplVersion+"\n{{- if .FabrikVersion}}\n\nrequire (\n"+
			"{{- range .FabrikModules}}\n\t{{.}} {{$.FabrikVersion}}\n{{- end}}\n)\n{{- end}}\n")

	for name, v := range tr.fixtures {
		write(t, root, "fabrik/internal/engine/testdata/"+name,
			"fixture description\n-- go.mod --\nmodule app\n\ngo "+v+"\n-- main.go --\npackage main\n")
	}

	for name, content := range tr.workflows {
		write(t, root, ".github/workflows/"+name, content)
	}

	cfg, err := modset.Load(root)
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	return cfg
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

func paths(findings []Finding) []string {
	var out []string
	for _, f := range findings {
		out = append(out, f.Path)
	}
	return out
}

func TestCheckClean(t *testing.T) {
	cfg := build(t, defaultTree())
	findings, err := Check(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("want no findings, got %v", findings)
	}
}

func TestCheckGoMod(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(t *testing.T, cfg *modset.Config)
		wantPath  string
		wantFound string
	}{
		{
			name: "workspace member wrong version",
			mutate: func(t *testing.T, cfg *modset.Config) {
				write(t, cfg.Root, "b/go.mod", "module example.com/b\n\ngo 1.25\n")
			},
			wantPath:  "b/go.mod",
			wantFound: "1.25",
		},
		{
			name: "workspace member missing directive",
			mutate: func(t *testing.T, cfg *modset.Config) {
				write(t, cfg.Root, "b/go.mod", "module example.com/b\n")
			},
			wantPath:  "b/go.mod",
			wantFound: missing,
		},
		{
			name: "tools module wrong version",
			mutate: func(t *testing.T, cfg *modset.Config) {
				write(t, cfg.Root, "internal/tools/go.mod", "module github.com/gofabrik/fabrik/internal/tools\n\ngo 1.25\n")
			},
			wantPath:  "internal/tools/go.mod",
			wantFound: "1.25",
		},
		{
			name: "tools module missing directive",
			mutate: func(t *testing.T, cfg *modset.Config) {
				write(t, cfg.Root, "internal/tools/go.mod", "module github.com/gofabrik/fabrik/internal/tools\n")
			},
			wantPath:  "internal/tools/go.mod",
			wantFound: missing,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := build(t, defaultTree())
			tt.mutate(t, cfg)
			findings, err := Check(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if len(findings) != 1 || findings[0].Path != tt.wantPath || findings[0].Found != tt.wantFound {
				t.Fatalf("want one finding at %s found=%s, got %v", tt.wantPath, tt.wantFound, findings)
			}
			if findings[0].Expected != "1.26" {
				t.Fatalf("want expected=1.26, got %+v", findings[0])
			}
		})
	}
}

func TestCheckTemplate(t *testing.T) {
	const path = "fabrik/templates/starter/go.mod.tmpl"
	tests := []struct {
		name      string
		mutate    func(t *testing.T, cfg *modset.Config)
		wantFound string
	}{
		{
			name: "wrong version",
			mutate: func(t *testing.T, cfg *modset.Config) {
				write(t, cfg.Root, path, "module {{.Module}}\n\ngo 1.24\n")
			},
			wantFound: "1.24",
		},
		{
			name: "missing directive",
			mutate: func(t *testing.T, cfg *modset.Config) {
				write(t, cfg.Root, path, "module {{.Module}}\n")
			},
			wantFound: missing,
		},
		{
			name: "file absent",
			mutate: func(t *testing.T, cfg *modset.Config) {
				if err := os.Remove(filepath.Join(cfg.Root, filepath.FromSlash(path))); err != nil {
					t.Fatal(err)
				}
			},
			wantFound: missing,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := build(t, defaultTree())
			tt.mutate(t, cfg)
			findings, err := Check(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if len(findings) != 1 || findings[0].Path != path || findings[0].Found != tt.wantFound {
				t.Fatalf("want one finding at %s found=%s, got %v", path, tt.wantFound, findings)
			}
		})
	}
}

func TestCheckFixture(t *testing.T) {
	const path = "fabrik/internal/engine/testdata/assets.txt"
	tests := []struct {
		name      string
		mutate    func(t *testing.T, cfg *modset.Config)
		wantFound string
	}{
		{
			name: "wrong version",
			mutate: func(t *testing.T, cfg *modset.Config) {
				write(t, cfg.Root, path, "fixture description\n-- go.mod --\nmodule app\n\ngo 1.23\n-- main.go --\npackage main\n")
			},
			wantFound: "1.23",
		},
		{
			name: "missing directive",
			mutate: func(t *testing.T, cfg *modset.Config) {
				write(t, cfg.Root, path, "fixture\n-- go.mod --\nmodule app\n")
			},
			wantFound: missing,
		},
		{
			name: "no go.mod section",
			mutate: func(t *testing.T, cfg *modset.Config) {
				write(t, cfg.Root, path, "fixture with no sections at all\n")
			},
			wantFound: missing,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := build(t, defaultTree())
			tt.mutate(t, cfg)
			findings, err := Check(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if len(findings) != 1 || findings[0].Path != path || findings[0].Found != tt.wantFound {
				t.Fatalf("want one finding at %s found=%s, got %v", path, tt.wantFound, findings)
			}
		})
	}
}

// TestCheckWorkflow covers every single-step shape the setup-go scanner must
// get right: each YAML scalar spelling (matching and mismatching), a step
// with no pin at all, a step that isn't setup-go, and the two structural
// bypasses a text scanner is prone to - a pin under env instead of with, and
// a commented-out uses line that must not be read as a step.
func TestCheckWorkflow(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		wantFound string // "" means no findings
	}{
		{
			name:    "single-quoted match",
			content: stepWorkflow("      - uses: actions/setup-go@v7\n        with:\n          go-version: '1.26'\n"),
		},
		{
			name:      "single-quoted mismatch",
			content:   stepWorkflow("      - uses: actions/setup-go@v7\n        with:\n          go-version: '1.25'\n"),
			wantFound: "1.25",
		},
		{
			name:    "double-quoted match",
			content: stepWorkflow("      - uses: actions/setup-go@v7\n        with:\n          go-version: \"1.26\"\n"),
		},
		{
			name:      "double-quoted mismatch",
			content:   stepWorkflow("      - uses: actions/setup-go@v7\n        with:\n          go-version: \"1.25\"\n"),
			wantFound: "1.25",
		},
		{
			// The trailing comment doubles as a regression check that yaml.v3
			// strips it rather than folding it into the scalar value.
			name:    "unquoted match with trailing comment",
			content: stepWorkflow("      - uses: actions/setup-go@v7\n        with:\n          go-version: 1.26 # pinned\n"),
		},
		{
			name:      "unquoted mismatch",
			content:   stepWorkflow("      - uses: actions/setup-go@v7\n        with:\n          go-version: 1.25\n"),
			wantFound: "1.25",
		},
		{
			name:      "deleted pin: no with block at all",
			content:   stepWorkflow("      - uses: actions/setup-go@v7\n"),
			wantFound: missing,
		},
		{
			name:    "a step that isn't setup-go needs no pin",
			content: stepWorkflow("      - run: echo hi\n"),
		},
		{
			name:      "a pin under env does not satisfy with.go-version",
			content:   stepWorkflow("      - uses: actions/setup-go@v7\n        env:\n          go-version: '1.26'\n"),
			wantFound: missing,
		},
		{
			name:    "a commented-out uses line is not a step",
			content: stepWorkflow("      # - uses: actions/setup-go@v7\n      - run: echo hi\n"),
		},
		{
			name:      "an empty pin counts as missing",
			content:   stepWorkflow("      - uses: actions/setup-go@v7\n        with:\n          go-version: ''\n"),
			wantFound: missing,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := defaultTree()
			tr.workflows = map[string]string{"ci.yml": tt.content}
			cfg := build(t, tr)
			findings, err := Check(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantFound == "" {
				if len(findings) != 0 {
					t.Fatalf("want no findings, got %v", findings)
				}
				return
			}
			if len(findings) != 1 || findings[0].Found != tt.wantFound {
				t.Fatalf("want one finding found=%s, got %v", tt.wantFound, findings)
			}
		})
	}
}

func TestCheckWorkflowMismatchNoToolchain(t *testing.T) {
	tr := defaultTree()
	tr.workflows["nightly.yml"] = workflowContent([]string{"1.26", "1.25", "1.26"})
	cfg := build(t, tr)
	findings, err := Check(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := ".github/workflows/nightly.yml:13" // second job's go-version line
	if len(findings) != 1 || findings[0].Path != want {
		t.Fatalf("want one finding at %s, got %v", want, findings)
	}
	if findings[0].Found != "1.25" || findings[0].Expected != "1.26" {
		t.Fatalf("want found=1.25 expected=1.26, got %+v", findings[0])
	}
}

func TestCheckWorkflowMultipleMismatches(t *testing.T) {
	tr := defaultTree()
	tr.workflows["ci.yml"] = workflowContent([]string{"1.20"})
	tr.workflows["nightly.yml"] = workflowContent([]string{"1.21", "1.26", "1.26"})
	cfg := build(t, tr)
	findings, err := Check(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got := paths(findings)
	want := []string{".github/workflows/ci.yml:8", ".github/workflows/nightly.yml:8"}
	if len(got) != len(want) {
		t.Fatalf("want findings at %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("want findings at %v, got %v", want, got)
		}
	}
}

func TestCheckWorkflowYamlExtensionScanned(t *testing.T) {
	tr := defaultTree()
	delete(tr.workflows, "ci.yml")
	delete(tr.workflows, "nightly.yml")
	tr.workflows["extra.yaml"] = workflowContent([]string{"1.20"})
	cfg := build(t, tr)
	findings, err := Check(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].Path != ".github/workflows/extra.yaml:8" {
		t.Fatalf("want one finding in extra.yaml, got %v", findings)
	}
}

func TestCheckWorkflowToolchainAwareAcceptsBothSpellings(t *testing.T) {
	tr := toolchainTree()
	tr.workflows = map[string]string{
		"ci.yml":      workflowContent([]string{"1.27.0-rc.2"}),
		"nightly.yml": workflowContent([]string{"go1.27rc2", "1.27.0-rc.2", "go1.27rc2"}),
	}
	cfg := build(t, tr)
	findings, err := Check(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("want no findings across equivalent spellings, got %v", findings)
	}
}

func TestCheckWorkflowToolchainAwareMismatch(t *testing.T) {
	tr := toolchainTree()
	tr.workflows = map[string]string{
		"ci.yml": workflowContent([]string{"1.27"}), // stale pin: go directive's minor, not the rc toolchain
	}
	cfg := build(t, tr)
	findings, err := Check(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].Found != "1.27" || findings[0].Expected != "1.27.0-rc.2" {
		t.Fatalf("want one finding found=1.27 expected=1.27.0-rc.2, got %v", findings)
	}
}

func TestCheckWorkflowNoToolchainComparesMajorMinor(t *testing.T) {
	tr := patchedGoTree()
	tr.workflows = map[string]string{"ci.yml": workflowContent([]string{"1.27"})}
	cfg := build(t, tr)
	findings, err := Check(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("want a bare major.minor pin to match a patched go directive, got %v", findings)
	}
}

func TestCheckWorkflowNoToolchainMajorMinorMismatchStillCaught(t *testing.T) {
	tr := patchedGoTree()
	tr.workflows = map[string]string{"ci.yml": workflowContent([]string{"1.26"})}
	cfg := build(t, tr)
	findings, err := Check(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].Found != "1.26" || findings[0].Expected != "1.27" {
		t.Fatalf("want one finding found=1.26 expected=1.27, got %v", findings)
	}
}

func TestCheckMultipleSimultaneousMismatches(t *testing.T) {
	tr := defaultTree()
	cfg := build(t, tr)
	write(t, cfg.Root, "b/go.mod", "module example.com/b\n")
	write(t, cfg.Root, "fabrik/templates/starter/go.mod.tmpl", "module {{.Module}}\n")
	write(t, cfg.Root, "fabrik/internal/engine/testdata/assets.txt", "no sections here\n")
	write(t, cfg.Root, ".github/workflows/ci.yml", "name: x\non: push\njobs:\n  job0:\n    steps:\n      - uses: actions/setup-go@v7\n")

	findings, err := Check(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		".github/workflows/ci.yml:6",
		"b/go.mod",
		"fabrik/internal/engine/testdata/assets.txt",
		"fabrik/templates/starter/go.mod.tmpl",
	}
	got := paths(findings)
	if len(got) != len(want) {
		t.Fatalf("want findings at %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("want findings at %v, got %v", want, got)
		}
	}
	for _, f := range findings {
		if f.Found != missing {
			t.Errorf("want every finding to report found=%s, got %+v", missing, f)
		}
	}
}

func TestCheckTemplateUnreadableIsAnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: file permissions are not enforced")
	}
	tr := defaultTree()
	cfg := build(t, tr)
	path := filepath.Join(cfg.Root, "fabrik", "templates", "starter", "go.mod.tmpl")
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(path, 0o600)
	if _, err := Check(cfg); err == nil {
		t.Fatal("want an error for a genuinely unreadable template, not a finding")
	}
}

func TestCheckFixtureUnreadableIsAnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: file permissions are not enforced")
	}
	tr := defaultTree()
	cfg := build(t, tr)
	path := filepath.Join(cfg.Root, "fabrik", "internal", "engine", "testdata", "assets.txt")
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(path, 0o600)
	if _, err := Check(cfg); err == nil {
		t.Fatal("want an error for a genuinely unreadable fixture, not a finding")
	}
}

func TestCanonicalVersion(t *testing.T) {
	tests := []struct {
		name string
		a, b string
	}{
		{"toolchain rc vs setup-go rc spelling", "go1.27rc2", "1.27.0-rc.2"},
		{"toolchain beta vs setup-go beta spelling", "go1.27beta1", "1.27.0-beta.1"},
		{"bare vs zero-patch", "1.26", "1.26.0"},
		{"go-prefixed bare vs zero-patch", "go1.26", "1.26.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			va, err := canonicalVersion(tt.a)
			if err != nil {
				t.Fatalf("canonicalVersion(%q): %v", tt.a, err)
			}
			vb, err := canonicalVersion(tt.b)
			if err != nil {
				t.Fatalf("canonicalVersion(%q): %v", tt.b, err)
			}
			if va != vb {
				t.Fatalf("canonicalVersion(%q)=%q, canonicalVersion(%q)=%q: want equal", tt.a, va, tt.b, vb)
			}
		})
	}
}

func TestCanonicalVersionRejectsGarbage(t *testing.T) {
	if _, err := canonicalVersion("not-a-version"); err == nil {
		t.Fatal("want an error for an unparseable version")
	}
}
