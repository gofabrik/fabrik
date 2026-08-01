package engine

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/fabrik/internal/genconfig"
	"github.com/gofabrik/fabrik/fabrik/internal/genfiles"
	"github.com/gofabrik/fabrik/gen"
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
	gomod := "module fixture\n\ngo 1.27\n\nreplace (\n\tgithub.com/gofabrik/fabrik/config => /tmp/x\n)\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o600); err != nil {
		t.Fatal(err)
	}
	replaces := map[string]string{
		"github.com/gofabrik/fabrik/config": "/elsewhere",
		"github.com/gofabrik/fabrik/router": "/local/router",
	}
	err := compileFixture(dir, dir, dir, map[string][]byte{"main.gen.go": []byte("package main\n\nfunc main() { missing() }\n")}, replaces)
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

// compileFixture builds generated source against local fabrik modules.
func compileFixture(dir, outDir, mainDir string, files map[string][]byte, replaces map[string]string) error {
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return err
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(outDir, name), data, 0o600); err != nil {
			return err
		}
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
	target, buildDir := ".", mainDir
	if mainDir == "" || outDir != mainDir {
		// Embedded output compiles as part of the whole module.
		target, buildDir = "./...", dir
	}
	// #nosec G204 -- the command and all arguments are controlled by this test
	build := exec.Command("go", "build", "-o", os.DevNull, target)
	build.Dir = buildDir
	build.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod", "GOTOOLCHAIN="+runtime.Version())
	if b, err := build.CombinedOutput(); err != nil {
		var src []byte
		for _, name := range slices.Sorted(maps.Keys(files)) {
			src = append(src, []byte("--- "+name+" ---\n")...)
			src = append(src, files[name]...)
		}
		return fmt.Errorf("go build failed: %w\n%s\n--- generated ---\n%s", err, b, src)
	}
	return nil
}

var fixtureModules = []struct{ token, path, rel string }{
	{"ROUTERDIR", "github.com/gofabrik/fabrik/router", "../../../router"},
	{"CONFIGDIR", "github.com/gofabrik/fabrik/config", "../../../config"},
	{"TEMPLATEDIR", "github.com/gofabrik/fabrik/templates", "../../../templates"},
	{"WEBDIR", "github.com/gofabrik/fabrik/web", "../../../web"},
	{"ASSETSDIR", "github.com/gofabrik/fabrik/assetmapper", "../../../assetmapper"},
	{"MIGRATIONSDIR", "github.com/gofabrik/fabrik/migrations", "../../../migrations"},
	{"JOBSDIR", "github.com/gofabrik/fabrik/jobs", "../../../jobs"},
	{"CLIDIR", "github.com/gofabrik/fabrik/cli", "../../../cli"},
	{"HTTPSERVERDIR", "github.com/gofabrik/fabrik/httpserver", "../../../httpserver"},
}

func writeFixtureTree(t *testing.T, dir string, ar *txtar.Archive) (wantGen map[string][]byte, wantDiags []byte, hasGen, hasDiags bool, replaces map[string]string) {
	t.Helper()
	subs := map[string]string{}
	replaces = map[string]string{}
	for _, m := range fixtureModules {
		abs, err := filepath.Abs(m.rel)
		if err != nil {
			t.Fatal(err)
		}
		subs[m.token] = abs
		replaces[m.path] = abs
	}
	for _, f := range ar.Files {
		if f.Name == "want/diags" {
			wantDiags, hasDiags = f.Data, true
			continue
		}
		if name, ok := strings.CutPrefix(f.Name, "want/"); ok && strings.HasSuffix(name, ".gen.go") {
			if wantGen == nil {
				wantGen = map[string][]byte{}
			}
			wantGen[name] = f.Data
			hasGen = true
			continue
		}
		path := filepath.Join(dir, f.Name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		// Fixtures resolve local module checkouts; only generated output imports config.
		data := f.Data
		for token, abs := range subs {
			data = bytes.ReplaceAll(data, []byte(token), []byte(abs))
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return wantGen, wantDiags, hasGen, hasDiags, replaces
}

func TestWireOptionsFullComments(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped under -short")
	}
	data, err := os.ReadFile(filepath.Join("testdata", "kitchen_sink.txt"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	_, _, _, _, replaces := writeFixtureTree(t, dir, txtar.Parse(data))

	res, err := WireOptions(dir, nil, Options{Comments: gen.CommentsFull})
	if err != nil {
		t.Fatalf("WireOptions: %v", err)
	}
	src := string(res.Src)
	// The select result inherits its enclosing provider's store.go:32 origin.
	for _, want := range []string{
		"// provider shared/http.go:5",
		"// provider:select store/store.go:23",
		"// provider store/store.go:32",
		"// hook shared/config.go:15",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("full output missing %q:\n%s", want, src)
		}
	}
	if strings.Contains(src, dir) {
		t.Errorf("origin comments leak absolute fixture paths:\n%s", src)
	}
	if err := compileFixture(dir, res.OutDir, res.MainDir, res.Files, replaces); err != nil {
		t.Error(err)
	}
}

func TestWireOptionsGraph(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "kitchen_sink.txt"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	writeFixtureTree(t, dir, txtar.Parse(data))

	plain, err := WireOptions(dir, nil, Options{})
	if err != nil {
		t.Fatalf("WireOptions: %v", err)
	}
	res, err := WireOptions(dir, nil, Options{Graph: true})
	if err != nil {
		t.Fatalf("WireOptions(graph): %v", err)
	}
	if plain.Graph != nil {
		t.Errorf("graph exported without the option")
	}
	if !bytes.Equal(plain.Src, res.Src) {
		t.Errorf("the graph option changed generated source:\n--- without ---\n%s--- with ---\n%s", plain.Src, res.Src)
	}
	if res.Graph == nil {
		t.Fatal("no graph exported")
	}

	first, err := json.MarshalIndent(res.Graph, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	again, err := WireOptions(dir, nil, Options{Graph: true})
	if err != nil {
		t.Fatalf("WireOptions(graph, second run): %v", err)
	}
	second, err := json.MarshalIndent(again.Graph, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("graph is not deterministic:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	if strings.Contains(string(first), dir) {
		t.Errorf("graph leaks absolute fixture paths:\n%s", first)
	}

	graph := res.Graph
	if graph.Version != 1 || graph.Module != "app" {
		t.Errorf("envelope = %d, %q", graph.Version, graph.Module)
	}
	var serve *gen.GraphFlow
	for i, f := range graph.Flows {
		if f.ID == "serve" {
			serve = &graph.Flows[i]
		}
	}
	if serve == nil {
		t.Fatalf("no serve flow in %+v", graph.Flows)
	}
	if serve.Fn != "buildServe" || len(serve.Roots) != 1 {
		t.Fatalf("serve flow = %+v", serve)
	}
	if serve.Roots[0].Binding == "" {
		t.Errorf("the httpserver root is an inline constructor and must reference a binding: %+v", serve.Roots)
	}
	var bound *gen.GraphBinding
	for i, b := range serve.Bindings {
		if strings.HasPrefix(b.Expr, "httpserver.New(") {
			bound = &serve.Bindings[i]
		}
	}
	if bound == nil {
		t.Fatalf("no httpserver binding in %+v", serve.Bindings)
	}
	if len(bound.Types) == 0 || bound.Types[0].Display != "*httpserver.Server" ||
		bound.Types[0].Canonical != "*github.com/gofabrik/fabrik/httpserver.Server" {
		t.Errorf("binding types = %+v", bound.Types)
	}

	byID := map[string]gen.GraphNode{}
	for _, n := range graph.Nodes {
		byID[n.ID] = n
	}
	pool := byID["serve/storePool"]
	if pool.Kind != "call" || pool.Fn != "store.NewPool" || pool.Type != "*store.Pool" ||
		pool.Directive != "provider" || pool.Pos != "store/store.go:44" {
		t.Errorf("provider node = %+v", pool)
	}
	sel := byID["serve/storeStore"]
	if sel.Kind != "select" || sel.Type != "store.Store" {
		t.Errorf("select node = %+v", sel)
	}
	child := byID["serve/storeStorePostgres"]
	if child.Parent != "serve/storeStore" || child.Case != "postgres" || child.Type != "store.Store" {
		t.Errorf("select child node = %+v", child)
	}
	if cfg := byID["serve/sharedConfig"]; cfg.Kind != "configLoad" || cfg.Type != "*shared.Config" {
		t.Errorf("config node = %+v", byID["serve/sharedConfig"])
	}
	found := false
	for _, e := range graph.Edges {
		if e.From == "serve/sharedHttpServer" && e.To == "serve/sharedConfig" {
			found = true
		}
	}
	if !found {
		t.Errorf("provider dependency edge missing:\n%s", first)
	}

	poolLabel := pool.Defines[0] + "\n" + pool.Type
	if pool.Cleanup != "" {
		poolLabel += " (cleanup)"
	}
	poolLabel += "\n" + pool.Directive + "  " + pool.Pos
	dot := graph.DOT()
	for _, want := range []string{
		"subgraph cluster_f",
		`label="buildServe";`,
		fmt.Sprintf("%q [label=%q];", pool.ID, poolLabel),
	} {
		if !strings.Contains(dot, want) {
			t.Errorf("DOT missing %q", want)
		}
	}
	if dot != graph.DOT() {
		t.Error("DOT rendering is not deterministic")
	}
	mmd := graph.Mermaid()
	mmdLabel := strings.ReplaceAll(strings.ReplaceAll(poolLabel, "\n", "<br/>"), "  ", " ")
	for _, want := range []string{"flowchart TB", `["buildServe"]`, mmdLabel} {
		if !strings.Contains(mmd, want) {
			t.Errorf("mermaid missing %q", want)
		}
	}
	if mmd != graph.Mermaid() {
		t.Error("mermaid rendering is not deterministic")
	}
}

func TestWireOptionsOutDirMatchesMainDir(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "kitchen_sink.txt"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	writeFixtureTree(t, dir, txtar.Parse(data))

	res, err := WireOptions(dir, nil, Options{})
	if err != nil {
		t.Fatalf("WireOptions: %v", err)
	}
	if res.OutDir != res.MainDir || res.OutDir == "" {
		t.Errorf("OutDir = %q, MainDir = %q, want them equal and non-empty", res.OutDir, res.MainDir)
	}
}

func TestWireOptionsBuildTag(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "kitchen_sink.txt"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	writeFixtureTree(t, dir, txtar.Parse(data))

	plain, err := WireOptions(dir, nil, Options{})
	if err != nil {
		t.Fatalf("WireOptions: %v", err)
	}
	if bytes.Contains(plain.Src, []byte("//go:build")) {
		t.Errorf("no buildtag set, but output carries a build constraint:\n%s", plain.Src)
	}
	tagged, err := WireOptions(dir, nil, Options{BuildTag: "e2e"})
	if err != nil {
		t.Fatalf("WireOptions(buildtag): %v", err)
	}
	if !bytes.HasPrefix(tagged.Src, []byte("//go:build e2e\n\n")) {
		t.Errorf("output does not start with the build constraint:\n%s", tagged.Src)
	}
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
	wantGen, wantDiags, hasGen, hasDiags, replaces := writeFixtureTree(t, dir, ar)

	opts, cdiags := genconfig.Resolve(dir, genconfig.Overrides{})
	if cdiags.HasFatal() {
		t.Fatalf("genconfig: %v", cdiags)
	}
	res, err := WireOptions(dir, nil, OptionsFrom(opts))
	if err != nil {
		t.Fatalf("Wire: %v", err)
	}
	gotDiags := renderDiags(res.Diags, dir)

	if len(res.Files) > 0 {
		// Regeneration must not let existing generated files affect naming or layout.
		if err := os.MkdirAll(res.OutDir, 0o750); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := genfiles.WriteSet(res.OutDir, res.Files, OptionsFrom(opts).Split); err != nil {
			t.Fatalf("WriteSet: %v", err)
		}
		again, err := WireOptions(dir, nil, OptionsFrom(opts))
		if err != nil {
			t.Fatalf("Wire (second run): %v", err)
		}
		for name := range res.Files {
			if !bytes.Equal(res.Files[name], again.Files[name]) {
				t.Errorf("generation is not deterministic (%s):\nfirst:\n%s\nsecond:\n%s", name, res.Files[name], again.Files[name])
			}
		}
		if len(again.Files) != len(res.Files) {
			t.Errorf("generation is not deterministic: file sets differ")
		}
		if !testing.Short() {
			if err := compileFixture(dir, res.OutDir, res.MainDir, res.Files, replaces); err != nil {
				t.Error(err)
			}
		}
	}

	if *update {
		if !t.Failed() {
			updateFixture(t, fixture, ar, res.Files, gotDiags)
		}
		return
	}

	if hasGen {
		for _, name := range slices.Sorted(maps.Keys(wantGen)) {
			got, ok := res.Files[name]
			if !ok {
				t.Errorf("missing generated file %s", name)
				continue
			}
			if !bytes.Equal(got, wantGen[name]) {
				t.Errorf("%s mismatch\n--- want ---\n%s--- got ---\n%s", name, wantGen[name], got)
			}
		}
		for _, name := range slices.Sorted(maps.Keys(res.Files)) {
			if _, ok := wantGen[name]; !ok {
				t.Errorf("unexpected generated file %s:\n%s", name, res.Files[name])
			}
		}
	}
	if !hasGen && len(res.Files) > 0 {
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

func updateFixture(t *testing.T, fixture string, ar *txtar.Archive, files map[string][]byte, diags string) {
	var kept []txtar.File
	for _, f := range ar.Files {
		if f.Name != "want/diags" && !strings.HasPrefix(f.Name, "want/") {
			kept = append(kept, f)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		kept = append(kept, txtar.File{Name: "want/" + name, Data: files[name]})
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

func TestWireOptionsFlagsStaleGeneratedFiles(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "kitchen_sink.txt"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	writeFixtureTree(t, dir, txtar.Parse(data))
	leftover := filepath.Join(dir, "shared", "fabrik.gen.go")
	if err := os.WriteFile(leftover, []byte("package shared\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Detect leftovers excluded from package loading by a build tag.
	tagged := filepath.Join(dir, "oldgen", "fabrik.gen.go")
	if err := os.MkdirAll(filepath.Dir(tagged), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tagged, []byte("//go:build e2e\n\npackage oldgen\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Generated files owned by a nested module are not leftovers.
	nested := filepath.Join(dir, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "go.mod"), []byte("module nested\n\ngo 1.27\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "main.gen.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := WireOptions(dir, nil, Options{})
	if err != nil {
		t.Fatalf("WireOptions: %v", err)
	}
	want := map[string]bool{leftover: true, tagged: true}
	if len(res.Stale) != len(want) {
		t.Fatalf("Stale = %v, want %v", res.Stale, want)
	}
	for _, f := range res.Stale {
		if !want[f] {
			t.Errorf("Stale contains unexpected %q", f)
		}
	}
	if res.Diags.HasFatal() {
		t.Errorf("Diags = %v, want no fatal from the generated-only directory", res.Diags)
	}
}

func TestWireOptionsRejectsSymlinkEscapingDir(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "kitchen_sink.txt"))
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	if r, err := filepath.EvalSymlinks(base); err == nil {
		base = r
	}
	dir := filepath.Join(base, "mod")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{dir, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFixtureTree(t, dir, txtar.Parse(data))
	if err := os.Symlink(outside, filepath.Join(dir, "gen")); err != nil {
		t.Fatal(err)
	}

	res, err := WireOptions(dir, nil, Options{Embedded: true, Dir: "gen/app", Package: "app"})
	if err != nil {
		t.Fatalf("WireOptions: %v", err)
	}
	found := false
	for _, d := range res.Diags {
		if strings.Contains(d.Message, "resolves outside the module") {
			found = true
		}
	}
	if !found {
		t.Errorf("Diags = %v, want the symlink-escape diagnostic", res.Diags)
	}
}

func TestResolveDirFollowsInModuleSymlink(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	resolved, contained, err := resolveDir(root, filepath.Join(root, "link", "app"))
	if err != nil {
		t.Fatalf("resolveDir: %v", err)
	}
	if !contained || resolved != filepath.Join(real, "app") {
		t.Errorf("resolveDir = %q, contained %v; want %q inside the module", resolved, contained, filepath.Join(real, "app"))
	}
}

func TestEmbeddedCycleDetectedThroughSymlinkedDir(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "embedded_import_cycle.txt"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	writeFixtureTree(t, dir, txtar.Parse(data))
	if err := os.Symlink(filepath.Join(dir, "appwire"), filepath.Join(dir, "wirelink")); err != nil {
		t.Fatal(err)
	}

	res, err := WireOptions(dir, nil, Options{Embedded: true, Dir: "wirelink", Package: "appwire"})
	if err != nil {
		t.Fatalf("WireOptions: %v", err)
	}
	found := false
	for _, d := range res.Diags {
		if strings.Contains(d.Message, "imports the output package app/appwire") {
			found = true
		}
	}
	if !found {
		t.Errorf("Diags = %v, want the cycle reported against the physical output package", res.Diags)
	}
}
