package gen

import (
	"bytes"
	"go/token"
	"go/types"
	"reflect"
	"testing"

	"github.com/gofabrik/fabrik/diag"
)

func TestDefinesAndUses(t *testing.T) {
	vars := map[string]bool{
		"webConfig": true, "webGreeter": true, "sharedConfig": true, "r": true,
	}

	sel := &Select{
		Var: "webGreeter", Iface: "web.Greeter",
		KeyExpr: "webConfig.Kind", FmtPkg: "fmt",
		Cases: []Case{{
			Value:  "hello",
			Result: Call{Fn: "web.NewHelloGreeter"},
		}},
	}
	if got := defines(sel); !reflect.DeepEqual(got, []string{"webGreeter"}) {
		t.Fatalf("defines(select) = %v", got)
	}
	if got := uses(sel, vars); !reflect.DeepEqual(got, []string{"webConfig"}) {
		t.Fatalf("uses(select) = %v, want [webConfig]", got)
	}

	lit := &StructLit{Var: "webAPI", Type: "web.API", Fields: []Field{{Name: "Greeter", Expr: "webGreeter"}}}
	if got := uses(lit, vars); !reflect.DeepEqual(got, []string{"webGreeter"}) {
		t.Fatalf("uses(structlit) = %v", got)
	}

	// Identifiers inside string literals must not count as uses.
	route := &Route{Router: "r", Kind: RouteMethod, Method: "GET", Pattern: "/webGreeter", Handler: "webAPI.Greet"}
	if got := uses(route, map[string]bool{"webGreeter": true, "r": true}); !reflect.DeepEqual(got, []string{"r"}) {
		t.Fatalf("uses(route) = %v, want [r] only - pattern string must not match", got)
	}

	// A node never uses what it defines; inline-if err calls define nothing.
	call := &Call{Fn: "shared.InitLogger", Args: []string{"sharedConfig"}, Err: ErrInline}
	if got := defines(call); got != nil {
		t.Fatalf("defines(inline call) = %v, want none", got)
	}
	if got := uses(call, vars); !reflect.DeepEqual(got, []string{"sharedConfig"}) {
		t.Fatalf("uses(call) = %v", got)
	}

	// Manual Uses entries participate.
	raw := &Raw{Base: Base{Uses: []string{"r"}}, Lines: []string{`addr := ":8080"`}, Defines: []string{"addr"}}
	if got := uses(raw, vars); !reflect.DeepEqual(got, []string{"r"}) {
		t.Fatalf("uses(raw) = %v, want manual [r]", got)
	}
}

func TestImportGroups(t *testing.T) {
	g := New()
	g.SetModule("demo")
	g.Import("net/http")
	g.Import("demo/web")
	g.Import("github.com/gofabrik/fabrik/router")
	g.Import("fmt")
	g.Import("demo/shared")

	var b bytes.Buffer
	g.writeImports(&b)
	want := `import (
"fmt"
"net/http"

"github.com/gofabrik/fabrik/router"

"demo/shared"
"demo/web"
)

`
	if b.String() != want {
		t.Fatalf("imports:\n%s\nwant:\n%s", b.String(), want)
	}
}

func TestLazyBindOwnerProvenance(t *testing.T) {
	g := New()
	g.SetDirective("config")
	g.BindLazy(types.Typ[types.String], "cfg", func() (string, diag.Diagnostics) {
		g.Stmt(PhaseConfig, "x := load()")
		return "x", nil
	})
	g.SetDirective("init") // the consumer materializes the binding
	if _, _, ok := g.Instance(types.Typ[types.String], "cfg"); !ok {
		t.Fatal("instance failed")
	}
	if got := g.nodes[0].base().Origin.Directive; got != "config" {
		t.Fatalf("lazy node directive = %q, want owner %q", got, "config")
	}
	if g.current != "init" {
		t.Fatalf("current directive = %q, want restored %q", g.current, "init")
	}
}

func TestPathBindingAPIs(t *testing.T) {
	g := New()
	g.SetDirective("templates")
	g.BindLazyPath("*x.Set", func() (string, diag.Diagnostics) {
		g.Stmt(PhaseWire, "s := load()")
		return "s", nil
	})
	if !g.HasBindingPath("*x.Set") || g.HasBindingPath("*x.Other") {
		t.Fatal("HasBindingPath wrong")
	}
	expr, ds, ok := g.InstancePath("*x.Set")
	if !ok || len(ds) != 0 || expr != "s" {
		t.Fatalf("InstancePath = %q %v %v", expr, ds, ok)
	}
	n := len(g.nodes)
	if expr, _, ok := g.InstancePath("*x.Set"); !ok || expr != "s" || len(g.nodes) != n {
		t.Fatal("InstancePath did not cache")
	}
	if _, _, ok := g.InstancePath("*x.Other"); ok {
		t.Fatal("unknown path resolved")
	}
}

func TestPathThenTypeResolutionSharesOneMaterialization(t *testing.T) {
	g := New()
	g.SetDirective("templates")
	builds := 0
	g.BindLazyPath("string", func() (string, diag.Diagnostics) { // path == TypeString(types.Typ[types.String])
		builds++
		g.Stmt(PhaseWire, "s := load()")
		return "s", nil
	})
	if expr, _, ok := g.InstancePath("string"); !ok || expr != "s" {
		t.Fatal("InstancePath failed")
	}
	expr, ds, ok := g.Instance(types.Typ[types.String], "")
	if !ok || len(ds) != 0 || expr != "s" {
		t.Fatalf("Instance after InstancePath = %q %v %v", expr, ds, ok)
	}
	if builds != 1 {
		t.Fatalf("build ran %d times, want one shared materialization", builds)
	}
	if expr, _, ok := g.Instance(types.Typ[types.String], ""); !ok || expr != "s" {
		t.Fatal("type binding not recorded")
	}
}

func TestInstancePathDiagnosedBuildIsNotACycle(t *testing.T) {
	g := New()
	builds := 0
	g.BindLazyPath("*x.Broken", func() (string, diag.Diagnostics) {
		builds++
		var ds diag.Diagnostics
		ds.Error(token.Position{}, "broken", "")
		return "", ds
	})
	if _, ds, ok := g.InstancePath("*x.Broken"); !ok || len(ds) != 1 || ds[0].Message != "broken" {
		t.Fatalf("first = %v %v", ds, ok)
	}
	// Diagnosed path builds are retryable.
	_, ds, ok := g.InstancePath("*x.Broken")
	if !ok || len(ds) != 1 || ds[0].Message != "broken" {
		t.Fatalf("second = %v %v, want the original diagnostic", ds, ok)
	}
	if builds != 2 {
		t.Fatalf("builds = %d", builds)
	}
}

func TestPanickingBuildLeavesStateClean(t *testing.T) {
	g := New()
	g.SetDirective("outer")
	first := true
	g.BindLazyPath("*x.Panicky", func() (string, diag.Diagnostics) {
		if first {
			first = false
			panic("boom")
		}
		return "v", nil
	})
	func() {
		defer func() { recover() }() // the engine's guard does this
		g.InstancePath("*x.Panicky")
	}()
	if g.current != "outer" {
		t.Fatalf("current = %q, provenance dirty after panic", g.current)
	}
	if len(g.materializing) != 0 {
		t.Fatalf("materializing = %v, cycle stack dirty after panic", g.materializing)
	}
	expr, ds, ok := g.InstancePath("*x.Panicky")
	if !ok || len(ds) != 0 || expr != "v" {
		t.Fatalf("retry = %q %v %v", expr, ds, ok)
	}
}

func TestDiagnosedTypeBuildReportsOnceAcrossConsumers(t *testing.T) {
	g := New()
	builds := 0
	g.BindLazy(types.Typ[types.Int], "", func() (string, diag.Diagnostics) {
		builds++
		var ds diag.Diagnostics
		ds.Error(token.Position{}, "cycle-ish", "")
		return "nil", ds
	})
	_, ds1, ok := g.Instance(types.Typ[types.Int], "")
	if !ok || len(ds1) != 1 {
		t.Fatalf("first = %v %v", ds1, ok)
	}
	// Diagnosed type builds dedupe shared dependency errors.
	expr, ds2, ok := g.Instance(types.Typ[types.Int], "")
	if !ok || len(ds2) != 0 || expr != "nil" {
		t.Fatalf("second = %q %v %v, want deduped reuse", expr, ds2, ok)
	}
	if builds != 1 {
		t.Fatalf("builds = %d", builds)
	}
}
