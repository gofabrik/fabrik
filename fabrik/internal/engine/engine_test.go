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
	"slices"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/diag"
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

// compileFixture builds generated source against local fabrik modules.
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

func writeFixtureTree(t *testing.T, dir string, ar *txtar.Archive) (wantGen, wantDiags []byte, hasGen, hasDiags bool, replaces map[string]string) {
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
	if err := compileFixture(dir, res.MainDir, res.Src, replaces); err != nil {
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
