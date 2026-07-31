package gen

import (
	"bytes"
	"go/token"
	"go/types"
	"reflect"
	"strings"
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

func TestUsesSelectorFieldsAreNotDependencies(t *testing.T) {
	vars := map[string]bool{"cfg": true, "store": true}
	n := &Assign{Var: "kind", Expr: "cfg.store"}
	if got := uses(n, vars); !reflect.DeepEqual(got, []string{"cfg"}) {
		t.Fatalf("uses = %v, want [cfg]: selector fields are not dependencies", got)
	}
	call := &Call{Var: "h", Fn: "r.Use", Args: []string{"mw"}}
	if got := uses(call, map[string]bool{"r": true, "mw": true, "Use": true}); !reflect.DeepEqual(got, []string{"r", "mw"}) {
		t.Fatalf("uses = %v, want [r mw]: method names are not dependencies", got)
	}
}

func TestUsesRawIsDeclaredOnly(t *testing.T) {
	vars := map[string]bool{"webConfig": true}
	n := &Raw{Lines: []string{"doSomething(webConfig)"}}
	if got := uses(n, vars); got != nil {
		t.Fatalf("uses = %v, want none: Raw lines are never scanned", got)
	}
}

func TestUsesFuncLitBindsLocals(t *testing.T) {
	vars := map[string]bool{
		"c": true, "m": true, "v": true, "k": true, "i": true, "n": true,
		"mgr": true, "items": true, "helper": true, "use": true,
	}
	closure := `func(c jobs.Context, m Msg) error {
		v := helper(c)
		const k = 1
		var n int
		for i := range items {
			_ = i
		}
		return use(v, m, k, n)
	}`
	call := &Call{Fn: "jobs.On", Args: []string{"mgr", `"name"`, closure}}
	if got := uses(call, vars); !reflect.DeepEqual(got, []string{"mgr", "helper", "items", "use"}) {
		t.Fatalf("uses = %v, want [mgr helper items use]: closure-bound names are excluded", got)
	}
}

func TestUsesFuncLitLexicalEdgeCases(t *testing.T) {
	cases := []struct {
		name string
		expr string
		vars []string
		want []string
	}{
		{"named results", "func() (res int) { res = f(a); return }", []string{"res", "f", "a"}, []string{"f", "a"}},
		{"declaration point sees outer", "func() { x := g(x); _ = x }", []string{"x", "g"}, []string{"g", "x"}},
		{"if init binding", "func() { if y := f(a); y > 0 { use(y) } }", []string{"y", "f", "a", "use"}, []string{"f", "a", "use"}},
		{"for init binding", "func() { for i := start; i < end; i++ { use(i) } }", []string{"i", "start", "end", "use"}, []string{"start", "end", "use"}},
		{"type switch binding", "func() { switch t := v.(type) { case int: use(t) } }", []string{"t", "v", "use"}, []string{"v", "use"}},
		{"type switch case types see outer", "func() { switch size := v.(type) { case [size]int: use(size) } }", []string{"size", "v", "use"}, []string{"v", "size", "use"}},
		{"nested block exits", "func() { { z := 1; _ = z }; use(z) }", []string{"z", "use"}, []string{"use", "z"}},
		{"nested funclits", "func(a int) { f := func(b int) { use(a, b, c) }; f(a) }", []string{"a", "b", "c", "f", "use"}, []string{"use", "c"}},
		{"param type dependency", "func(_ [size]int) { body() }", []string{"size", "body"}, []string{"size", "body"}},
		{"param type sees outer despite name collision", "func(size [size]int) { use(size) }", []string{"size", "use"}, []string{"size", "use"}},
		{"result type dependency", "func() (r [size]int) { return }", []string{"size", "r"}, []string{"size"}},
		{"switch init binding", "func() { switch s := f(a); s { case b: use(s) } }", []string{"s", "f", "a", "b", "use"}, []string{"f", "a", "b", "use"}},
		{"var declaration point sees outer", "func() { var x = g(x); _ = x }", []string{"x", "g"}, []string{"g", "x"}},
		{"const declaration point", "func() { const k = 1; use(k) }", []string{"k", "use"}, []string{"use"}},
		{"local type declaration", "func() { type dep int; _ = dep(0); use(x) }", []string{"dep", "use", "x"}, []string{"use", "x"}},
	}
	for _, tc := range cases {
		vars := map[string]bool{}
		for _, v := range tc.vars {
			vars[v] = true
		}
		n := &Assign{Var: "fn", Expr: tc.expr}
		if got := uses(n, vars); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: uses = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestUsesNestedSelectAggregates(t *testing.T) {
	vars := map[string]bool{"outerKey": true, "innerKey": true, "deepDep": true}
	inner := &Select{
		Var: "innerVal", Iface: "web.Inner", KeyExpr: "innerKey", FmtPkg: "fmt",
		Cases: []Case{{Value: "x", Result: Call{Var: "innerX", Fn: "mk", Args: []string{"deepDep"}}}},
	}
	outer := &Select{
		Var: "outerVal", Iface: "web.Outer", KeyExpr: "outerKey", FmtPkg: "fmt",
		Cases: []Case{{Value: "y", Body: []Node{inner}, Result: Call{Var: "outerY", Fn: "wrap", Args: []string{"innerVal"}}}},
	}
	if got := uses(outer, vars); !reflect.DeepEqual(got, []string{"outerKey", "innerKey", "deepDep"}) {
		t.Fatalf("uses = %v, want [outerKey innerKey deepDep]: nested Select recurses fully", got)
	}
}

func TestRenderRejectsEmptyExpressionField(t *testing.T) {
	g := New()
	g.SetDirective("web")
	g.Node(&Call{Var: "v", Fn: "mk", Args: []string{""}})
	if _, err := g.Render(); err == nil || !strings.Contains(err.Error(), "web") {
		t.Fatalf("Render error = %v, want attributed error for empty expression field", err)
	}
}

func TestRenderRejectsNestedSelectBadExpression(t *testing.T) {
	g := New()
	g.SetDirective("web")
	inner := &Select{Var: "iv", Iface: "web.I", KeyExpr: "k", FmtPkg: "fmt",
		Cases: []Case{{Value: "x", Result: Call{Var: "ix", Fn: "mk", Args: []string{""}}}}}
	g.Node(&Select{Var: "ov", Iface: "web.O", KeyExpr: "k2", FmtPkg: "fmt",
		Cases: []Case{{Value: "y", Body: []Node{inner}, Result: Call{Var: "oy", Fn: "wrap"}}}})
	if _, err := g.Render(); err == nil || !strings.Contains(err.Error(), "web") {
		t.Fatalf("Render error = %v, want attributed error from nested Select child", err)
	}
}

func TestUsesCompositeLiteralKeys(t *testing.T) {
	vars := map[string]bool{"Greeter": true, "g2": true, "routeName": true, "h": true}
	named := &Assign{Var: "api", Expr: "web.API{Greeter: g2}"}
	if got := uses(named, vars); !reflect.DeepEqual(got, []string{"g2"}) {
		t.Fatalf("uses = %v, want [g2]: named-type identifier keys are field names", got)
	}
	mapped := &Assign{Var: "mux", Expr: "map[string]web.Handler{routeName: h}"}
	if got := uses(mapped, vars); !reflect.DeepEqual(got, []string{"routeName", "h"}) {
		t.Fatalf("uses = %v, want [routeName h]: map keys are expressions", got)
	}
}

func TestUsesSelectAggregatesChildren(t *testing.T) {
	vars := map[string]bool{"webConfig": true, "branchCfg": true, "dep": true}
	sel := &Select{
		Var: "g", Iface: "web.Greeter", KeyExpr: "webConfig.Kind", FmtPkg: "fmt",
		Cases: []Case{{
			Value:  "fancy",
			Body:   []Node{&Assign{Var: "branchCfg", Expr: "loadCfg(dep)"}},
			Result: Call{Var: "gFancy", Fn: "web.NewFancy", Args: []string{"branchCfg"}},
		}},
	}
	if got := uses(sel, vars); !reflect.DeepEqual(got, []string{"webConfig", "dep"}) {
		t.Fatalf("uses = %v, want [webConfig dep]: children aggregate, their defines are internal", got)
	}
}

func TestUsesCtxDetectsDeclaredAndExpressionUses(t *testing.T) {
	if !usesCtx([]Node{&Call{Fn: "hook.Setup", Args: []string{"ctx"}}}, "ctx") {
		t.Fatal("ctx in call args not detected")
	}
	if !usesCtx([]Node{&Raw{Base: Base{Uses: []string{"ctx"}}, Lines: []string{"x(ctx)"}}}, "ctx") {
		t.Fatal("declared ctx use not detected")
	}
	if usesCtx([]Node{&Raw{Lines: []string{"x(ctx)"}}}, "ctx") {
		t.Fatal("undeclared Raw text must not count as a ctx use")
	}
}

func TestRenderRejectsUnparsableExpressionField(t *testing.T) {
	g := New()
	g.SetDirective("web")
	g.Node(&Assign{Var: "v", Expr: "func("})
	if _, err := g.Render(); err == nil || !strings.Contains(err.Error(), "web") {
		t.Fatalf("Render error = %v, want unparsable-expression error naming the directive", err)
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
		g.Node(&Raw{Base: Base{Phase: PhaseConfig}, Lines: []string{"x := load()"}})
		return "x", nil
	})
	g.SetDirective("hook") // the consumer materializes the binding
	if _, _, ok := g.Instance(types.Typ[types.String], "cfg"); !ok {
		t.Fatal("instance failed")
	}
	if got := g.nodes[0].base().Origin.Directive; got != "config" {
		t.Fatalf("lazy node directive = %q, want owner %q", got, "config")
	}
	if g.current != "hook" {
		t.Fatalf("current directive = %q, want restored %q", g.current, "hook")
	}
}

func TestPathBindingAPIs(t *testing.T) {
	g := New()
	g.SetDirective("templates")
	g.BindLazyPath("*x.Set", func() (string, diag.Diagnostics) {
		g.Node(&Raw{Base: Base{Phase: PhaseWire}, Lines: []string{"s := load()"}})
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
		g.Node(&Raw{Base: Base{Phase: PhaseWire}, Lines: []string{"s := load()"}})
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

func TestCycleChainRendersBindingNames(t *testing.T) {
	pkg := typecheckScopePkg(t, "example.com/app", `package app

type A struct{}
type B struct{}
`)
	aT := types.NewPointer(pkg.Scope().Lookup("A").Type())
	bT := types.NewPointer(pkg.Scope().Lookup("B").Type())
	g := New()
	g.BindLazy(aT, "primary", func() (string, diag.Diagnostics) {
		_, ds, _ := g.Instance(bT, "secondary")
		return "a", ds
	})
	g.BindLazy(bT, "secondary", func() (string, diag.Diagnostics) {
		_, ds, _ := g.Instance(aT, "primary")
		return "b", ds
	})
	_, ds, ok := g.Instance(aT, "primary")
	if !ok || len(ds) != 1 {
		t.Fatalf("cycle = %v %v", ds, ok)
	}
	want := "provider cycle: *app.A (primary) -> *app.B (secondary) -> *app.A (primary)"
	if ds[0].Message != want {
		t.Fatalf("message = %q, want %q", ds[0].Message, want)
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
	_, ds, ok := g.InstancePath("*x.Panicky")
	if ok || len(ds) != 1 || !strings.Contains(ds[0].Message, `directive "outer" panicked`) {
		t.Fatalf("panicking build = %v %v, want attributed diagnostic", ds, ok)
	}
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

func TestFindUnparsableIncludesScopesAndSelectChildren(t *testing.T) {
	badRaw := func(directive, text string) *Raw {
		return &Raw{Base: Base{Origin: Origin{Directive: directive}}, Lines: []string{text}}
	}
	badResult := func(directive string) Call {
		return Call{
			Base: Base{Origin: Origin{Directive: directive}},
			Var:  "v-",
			Fn:   "makeV",
			Err:  ErrReturn,
		}
	}

	tests := []struct {
		name      string
		configure func(*Gen)
		directive string
		text      string
	}{
		{
			name: "global node",
			configure: func(g *Gen) {
				g.nodes = append(g.nodes, badRaw("global", "if {"))
			},
			directive: "global",
			text:      "if {",
		},
		{
			name: "scope node",
			configure: func(g *Gen) {
				s := g.AddScope("build", token.Position{})
				s.nodes = append(s.nodes, &Call{
					Base:    Base{Origin: Origin{Directive: "cleanup"}},
					Var:     "v",
					Fn:      "makeV",
					Cleanup: "closeV",
				})
				s.nodes = append(s.nodes, badRaw("scoped", "for {"))
			},
			directive: "scoped",
			text:      "for {",
		},
		{
			name: "select body",
			configure: func(g *Gen) {
				g.nodes = append(g.nodes, &Select{
					Base:    Base{Origin: Origin{Directive: "select"}},
					Var:     "v",
					Iface:   "I",
					KeyExpr: "key",
					FmtPkg:  "fmt",
					Cases: []Case{{
						Value:  "x",
						Body:   []Node{badRaw("body", "switch {")},
						Result: Call{Var: "vx", Fn: "newX"},
					}},
				})
			},
			directive: "body",
			text:      "switch {",
		},
		{
			name: "scoped select result",
			configure: func(g *Gen) {
				s := g.AddScope("build", token.Position{})
				s.nodes = append(s.nodes, &Select{
					Base:    Base{Origin: Origin{Directive: "select"}},
					Var:     "v",
					Iface:   "I",
					KeyExpr: "key",
					FmtPkg:  "fmt",
					Cases: []Case{{
						Value:  "x",
						Result: badResult("result"),
					}},
				})
			},
			directive: "result",
			text:      "v-, err := makeV()\nif err != nil {\nreturn err\n}\nv = v-",
		},
		{
			// Zero-Base body nodes inherit the parent directive.
			name: "bare select body attributes to parent",
			configure: func(g *Gen) {
				g.nodes = append(g.nodes, &Select{
					Base:    Base{Origin: Origin{Directive: "provider:select"}},
					Var:     "v",
					Iface:   "I",
					KeyExpr: "key",
					FmtPkg:  "fmt",
					Cases: []Case{{
						Value:  "x",
						Body:   []Node{&Raw{Lines: []string{"go go go"}}},
						Result: Call{Var: "vx", Fn: "newX"},
					}},
				})
			},
			directive: "provider:select",
			text:      "go go go",
		},
		{
			// The probe must match the scope's return arity and unwind path.
			name: "scoped probe uses the scope error context",
			configure: func(g *Gen) {
				s := g.AddScope("build", token.Position{})
				s.zeros = []string{"nil"}
				s.hasCleanup = true
				s.nodes = append(s.nodes, &Call{
					Base:    Base{Origin: Origin{Directive: "cleanup"}},
					Var:     "v",
					Fn:      "makeV",
					Err:     ErrReturn,
					Cleanup: "vClose",
				})
				s.nodes = append(s.nodes, &Call{
					Base: Base{Origin: Origin{Directive: "broken"}},
					Var:  "w-",
					Fn:   "makeW",
					Err:  ErrReturn,
				})
			},
			directive: "broken",
			text:      "w-, err := makeW()\nif err != nil {\nreturn nil, unwind(err)\n}",
		},
		{
			// Zero-Base results inherit the Select directive.
			name: "bare select result attributes to parent",
			configure: func(g *Gen) {
				g.nodes = append(g.nodes, &Select{
					Base:    Base{Origin: Origin{Directive: "provider:select"}},
					Var:     "v",
					Iface:   "I",
					KeyExpr: "key",
					FmtPkg:  "fmt",
					Cases: []Case{{
						Value:  "x",
						Result: Call{Var: "v-", Fn: "makeV", Err: ErrReturn},
					}},
				})
			},
			directive: "provider:select",
			text:      "v-, err := makeV()\nif err != nil {\nreturn err\n}\nv = v-",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := New()
			tc.configure(g)
			directive, text, found := g.findUnparsable()
			if !found || directive != tc.directive || text != tc.text {
				t.Fatalf("findUnparsable = %q, %q, %v; want %q, %q, true",
					directive, text, found, tc.directive, tc.text)
			}
		})
	}
}
