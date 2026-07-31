package gen

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"reflect"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/diag"
)

func typecheckScopePkg(t *testing.T, pkgPath, src string) *types.Package {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "pkg.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	conf := types.Config{Importer: importer.Default()}
	pkg, err := conf.Check(pkgPath, fset, []*ast.File{f}, nil)
	if err != nil {
		t.Fatalf("typecheck: %v", err)
	}
	return pkg
}

type scopeWorld struct {
	g      *Gen
	store  types.Type
	cache  types.Type
	failAt string
}

func newScopeWorld(t *testing.T) *scopeWorld {
	t.Helper()
	pkg := typecheckScopePkg(t, "example.com/app", `package app

type Store struct{}

type Cache struct{}
`)
	w := &scopeWorld{
		g:     New(),
		store: types.NewPointer(pkg.Scope().Lookup("Store").Type()),
		cache: types.NewPointer(pkg.Scope().Lookup("Cache").Type()),
	}
	w.g.SetModule("demo")
	g := w.g
	g.BindLazy(w.store, "", func() (string, diag.Diagnostics) {
		if w.failAt == "store" {
			var ds diag.Diagnostics
			ds.Error(token.Position{Filename: "app.go", Line: 1, Column: 1}, "store is broken", "")
			return "", ds
		}
		v := g.Var("conn")
		c := g.Var(v + "Close")
		g.Node(&Call{
			Base:    Base{Phase: PhaseWire},
			Var:     v,
			Fn:      g.Import("example.com/app") + ".NewStore",
			Args:    []string{g.Context()},
			Err:     ErrReturn,
			Cleanup: c,
			ErrsPkg: g.Import("errors"),
		})
		return v, nil
	})
	g.BindLazy(w.cache, "", func() (string, diag.Diagnostics) {
		store, ds, ok := g.Instance(w.store, "")
		if !ok {
			return "", ds
		}
		v := g.Var("cache")
		g.Node(&Call{
			Base: Base{Phase: PhaseWire},
			Var:  v,
			Fn:   g.Import("example.com/app") + ".NewCache",
			Args: []string{store},
			Err:  ErrReturn,
		})
		return v, ds
	})
	return w
}

func renderScopes(t *testing.T, g *Gen) string {
	t.Helper()
	if ds := g.WalkFlows(); ds.HasFatal() {
		t.Fatalf("WalkFlows: %v", ds)
	}
	if ds := g.PlanFragments(); ds.HasFatal() {
		t.Fatalf("PlanFragments: %v", ds)
	}
	out, err := g.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return string(out)
}

func TestScopeBuildsItsSubtree(t *testing.T) {
	w := newScopeWorld(t)
	ping := w.g.AddScope("buildPing", token.Position{}, ScopeRoot{Type: w.cache})
	w.g.AddCommandFunc(CommandFunc{Name: "ping", Fn: "app.Ping", Scope: ping})
	src := renderScopes(t, w.g)

	want := `Run: func(ctx cli.Context) (err error) {
					conn, connClose, err := app.NewStore(ctx)
					if err != nil {
						return err
					}
					if connClose != nil {
						defer func() {
							err = errors.Join(err, connClose())
						}()
					}
					cache, err := app.NewCache(conn)
					if err != nil {
						return err
					}
					return app.Ping(ctx, cache)
				},`
	if !strings.Contains(src, want) {
		t.Fatalf("buildPing shape mismatch.\n--- want ---\n%s\n--- got ---\n%s", want, src)
	}
}

func TestScopesConstructIndependently(t *testing.T) {
	w := newScopeWorld(t)
	sa := w.g.AddScope("buildA", token.Position{}, ScopeRoot{Type: w.store})
	w.g.AddCommandFunc(CommandFunc{Name: "a", Fn: "app.A", Scope: sa})
	sb := w.g.AddScope("buildB", token.Position{}, ScopeRoot{Type: w.store})
	w.g.AddCommandFunc(CommandFunc{Name: "b", Fn: "app.B", Scope: sb})
	src := renderScopes(t, w.g)

	if got := strings.Count(src, "app.NewStore(ctx)"); got != 2 {
		t.Fatalf("store constructed %d times, want once per flow:\n%s", got, src)
	}
	if strings.Contains(src, "conn2") {
		t.Fatalf("flow-local names leaked across flows:\n%s", src)
	}
	if got := strings.Count(src, "return app.A(ctx, conn)"); got != 1 {
		t.Fatalf("command a consumes its own conn %d times, want 1:\n%s", got, src)
	}
	if got := strings.Count(src, "return app.B(ctx, conn)"); got != 1 {
		t.Fatalf("command b consumes its own conn %d times, want 1:\n%s", got, src)
	}
}

func TestScopeWithoutCleanupOmitsSlot(t *testing.T) {
	w := newScopeWorld(t)
	// Cache depends on the cleanup-bearing store, so use a separate root.
	pkg := typecheckScopePkg(t, "example.com/flag", "package flag\n\ntype Flags struct{}\n")
	flags := types.NewPointer(pkg.Scope().Lookup("Flags").Type())
	g := w.g
	g.BindLazy(flags, "", func() (string, diag.Diagnostics) {
		v := g.Var("flags")
		g.Node(&Call{Base: Base{Phase: PhaseWire}, Var: v, Fn: g.Import("example.com/flag") + ".Parse"})
		return v, nil
	})
	sf := g.AddScope("buildFlags", token.Position{}, ScopeRoot{Type: flags})
	g.AddCommandFunc(CommandFunc{Name: "flags", Fn: "app.Flags", Scope: sf})
	src := renderScopes(t, g)

	if !strings.Contains(src, "flags := flag.Parse()") {
		t.Fatalf("cleanup-free construction should inline as a bare assignment:\n%s", src)
	}
	if strings.Contains(src, "cleanup") {
		t.Fatalf("cleanup machinery composed in a cleanup-free flow:\n%s", src)
	}
}

func TestScopeContextBindsToParam(t *testing.T) {
	w := newScopeWorld(t)
	ping := w.g.AddScope("buildPing", token.Position{}, ScopeRoot{Type: w.store})
	w.g.AddCommandFunc(CommandFunc{Name: "ping", Fn: "app.Ping", Scope: ping})
	src := renderScopes(t, w.g)

	if !strings.Contains(src, "app.NewStore(ctx)") {
		t.Fatalf("flow Context() should bind to the command's ctx param:\n%s", src)
	}
	if got := strings.Count(src, "context.Background()"); got != 1 {
		t.Fatalf("context.Background() appears %d times, want only the signal shell's:\n%s", got, src)
	}
}

func TestValidationPassIsIsolatedAndDeterministic(t *testing.T) {
	w := newScopeWorld(t)
	ds := w.g.RunValidationPass()
	if ds.HasFatal() {
		t.Fatalf("validation pass: %v", ds)
	}
	out, err := w.g.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	fresh := newScopeWorld(t)
	base, err := fresh.g.Render()
	if err != nil {
		t.Fatalf("fresh render: %v", err)
	}
	if string(out) != string(base) {
		t.Fatalf("validation pass changed output.\n--- with pass ---\n%s\n--- without ---\n%s", out, base)
	}
	if strings.Contains(string(out), "conn") || strings.Contains(string(out), "example.com/app") {
		t.Fatalf("validation materialization leaked into output:\n%s", out)
	}
}

func TestValidationPassReportsDiagnostics(t *testing.T) {
	w := newScopeWorld(t)
	w.failAt = "store"
	ds := w.g.RunValidationPass()
	if !ds.HasFatal() {
		t.Fatalf("validation pass missed the broken provider: %v", ds)
	}
	found := false
	for _, d := range ds {
		if strings.Contains(d.Message, "store is broken") {
			found = true
		}
	}
	if !found {
		t.Fatalf("diagnostics = %v, want the provider error surfaced", ds)
	}
}

func TestValidationPassReportsLazyBindingPanic(t *testing.T) {
	g := New()
	g.SetDirective("test:provider")
	g.BindLazy(types.Typ[types.Int], "", func() (string, diag.Diagnostics) {
		panic("duplicate bind")
	})

	ds := g.RunValidationPass()
	if !ds.HasFatal() || len(ds) != 1 {
		t.Fatalf("validation pass diagnostics = %v, want one fatal diagnostic", ds)
	}
	for _, want := range []string{`directive "test:provider"`, "duplicate bind"} {
		if !strings.Contains(ds[0].Message, want) {
			t.Errorf("diagnostic %q missing %q", ds[0].Message, want)
		}
	}
}

func TestZeroExprKinds(t *testing.T) {
	pkg := typecheckScopePkg(t, "example.com/z", `package z

type S struct{}

type I interface{}
`)
	g := New()
	cases := []struct {
		t    types.Type
		want string
	}{
		{types.NewPointer(pkg.Scope().Lookup("S").Type()), "nil"},
		{pkg.Scope().Lookup("I").Type(), "nil"},
		{types.Typ[types.String], `""`},
		{types.Typ[types.UnsafePointer], "nil"},
		{types.Typ[types.Bool], "false"},
		{types.Typ[types.Int], "0"},
		{pkg.Scope().Lookup("S").Type(), "z.S{}"},
	}
	for _, c := range cases {
		if got := zeroExpr(g, c.t); got != c.want {
			t.Errorf("zeroExpr(%s) = %q, want %q", c.t, got, c.want)
		}
	}
}

func TestErrCtxTails(t *testing.T) {
	scoped := &errCtx{zeros: "nil, ", unwind: true}
	call := &Call{Var: "v", Fn: "build", Err: ErrReturn}
	want := []string{
		"v, err := build()",
		"if err != nil {",
		"return nil, unwind(err)",
		"}",
	}
	if got := renderNode(call, scoped); !reflect.DeepEqual(got, want) {
		t.Fatalf("scoped ErrReturn = %v, want %v", got, want)
	}
	wantDefault := []string{"v, err := build()", "if err != nil {", "return err", "}"}
	if got := renderNode(call, nil); !reflect.DeepEqual(got, wantDefault) {
		t.Fatalf("default ErrReturn = %v, want %v", got, wantDefault)
	}

	inline := &Call{Fn: "setup", Err: ErrInline}
	wantInline := []string{
		"if err := setup(); err != nil {",
		"return nil, unwind(err)",
		"}",
	}
	if got := renderNode(inline, scoped); !reflect.DeepEqual(got, wantInline) {
		t.Fatalf("scoped ErrInline = %v, want %v", got, wantInline)
	}

	sel := &Select{Var: "g", Iface: "web.G", KeyExpr: "k", FmtPkg: "fmt"}
	got := renderNode(sel, scoped)
	if last := got[len(got)-2]; last != `return nil, unwind(fmt.Errorf("no web.G implementation for %q", k))` {
		t.Fatalf("scoped select tail = %q", last)
	}
	noCleanup := &errCtx{zeros: "nil, "}
	got = renderNode(sel, noCleanup)
	if last := got[len(got)-2]; last != `return nil, fmt.Errorf("no web.G implementation for %q", k)` {
		t.Fatalf("scoped select without cleanup = %q", last)
	}
	got = renderNode(sel, nil)
	if last := got[len(got)-2]; last != `return fmt.Errorf("no web.G implementation for %q", k)` {
		t.Fatalf("default select tail = %q", last)
	}
}

func TestRawCheckRendersContextTail(t *testing.T) {
	raw := &Raw{Lines: []string{"err = validate(v)"}, Check: true}
	wantDefault := []string{"err = validate(v)", "if err != nil {", "return err", "}"}
	if got := renderNode(raw, nil); !reflect.DeepEqual(got, wantDefault) {
		t.Fatalf("default Raw.Check = %v, want %v", got, wantDefault)
	}
	scoped := &errCtx{zeros: "nil, "}
	wantScoped := []string{"err = validate(v)", "if err != nil {", "return nil, err", "}"}
	if got := renderNode(raw, scoped); !reflect.DeepEqual(got, wantScoped) {
		t.Fatalf("scoped Raw.Check = %v, want %v", got, wantScoped)
	}
}

func TestNodesHaveCheckRecursesSelectChildren(t *testing.T) {
	sel := &Select{Var: "v", Iface: "I", KeyExpr: "k", FmtPkg: "fmt",
		Cases: []Case{{Value: "x", Body: []Node{&Raw{Lines: []string{"err = f()"}, Check: true}}}}}
	if !nodesHaveCheck([]Node{sel}) {
		t.Fatal("Raw.Check inside a Select body must trigger err declaration")
	}
	if nodesHaveCheck([]Node{&Raw{Lines: []string{"x := 1"}}}) {
		t.Fatal("no Check present, no declaration")
	}
}

func TestRenderRejectsRawReturn(t *testing.T) {
	g := New()
	g.SetDirective("jobs")
	g.Node(&Raw{Lines: []string{"if bad {", "return err", "}"}})
	if _, err := g.Render(); err == nil || !strings.Contains(err.Error(), "jobs") {
		t.Fatalf("Render error = %v, want raw-return contract error naming the directive", err)
	}
	g2 := New()
	g2.SetDirective("jobs")
	g2.Node(&Raw{Lines: []string{"h := func() error {", "return nil", "}", "_ = h"}, Defines: []string{"h"}})
	if _, err := g2.Render(); err != nil {
		t.Fatalf("Render error = %v, returns inside function literals are legal", err)
	}
}

// Scoped resolution must reuse bindings published by an active lazy builder.
func TestScopeSelfPublishingLazyBind(t *testing.T) {
	w := newScopeWorld(t)
	g := w.g
	pkg := typecheckScopePkg(t, "example.com/srv", "package srv\n\ntype Server struct{}\n")
	srv := types.NewPointer(pkg.Scope().Lookup("Server").Type())
	g.BindLazy(srv, "", func() (string, diag.Diagnostics) {
		expr := g.Import("example.com/srv") + ".New()"
		g.Bind(srv, "", expr)
		if again, _, ok := g.Instance(srv, ""); !ok || again != expr {
			t.Fatalf("published bind not visible during build: %q ok=%v", again, ok)
		}
		return expr, nil
	})
	g.AddScope("buildSrv", token.Position{}, ScopeRoot{Type: srv})
	src := renderScopes(t, g)
	if strings.Contains(src, "func buildSrv") {
		t.Fatalf("nodeless scope must not emit a build function:\n%s", src)
	}
}

func TestHasBindingMirrorsActiveScopeResolution(t *testing.T) {
	typ := types.Typ[types.String]

	t.Run("scope eager binding is visible", func(t *testing.T) {
		g := New()
		g.enterScope(&Scope{}, false)
		g.Bind(typ, "name", "scoped")
		if !g.HasBinding(typ, "name") {
			t.Fatal("HasBinding missed the active scope binding")
		}
	})

	t.Run("global eager binding is hidden", func(t *testing.T) {
		g := New()
		g.Bind(typ, "name", "global")
		g.enterScope(&Scope{}, false)
		if g.HasBinding(typ, "name") {
			t.Fatal("HasBinding reported a global eager binding hidden from the active scope")
		}
	})

	t.Run("shared lazy binding is visible", func(t *testing.T) {
		g := New()
		g.BindLazy(typ, "name", func() (string, diag.Diagnostics) { return "lazy", nil })
		g.enterScope(&Scope{}, false)
		if !g.HasBinding(typ, "name") {
			t.Fatal("HasBinding missed a shared lazy binding")
		}
	})

	t.Run("path materialization follows flow visibility", func(t *testing.T) {
		g := New()
		g.BindPath("string", "global")
		if !g.HasBinding(typ, "") {
			t.Fatal("HasBinding missed the default flow path materialization")
		}
		g.enterScope(&Scope{}, false)
		if g.HasBinding(typ, "") {
			t.Fatal("HasBinding reported a default flow path materialization in the active scope")
		}
		g.BindPath("string", "scoped")
		if !g.HasBinding(typ, "") {
			t.Fatal("HasBinding missed the active scope path materialization")
		}
	})
}

func TestHasBindingPathMirrorsActiveScopeResolution(t *testing.T) {
	const path = "*example.com/app.Store"

	t.Run("scope materialization is visible", func(t *testing.T) {
		g := New()
		g.enterScope(&Scope{}, false)
		g.BindPath(path, "scoped")
		if !g.HasBindingPath(path) {
			t.Fatal("HasBindingPath missed the active scope binding")
		}
	})

	t.Run("global materialization is hidden", func(t *testing.T) {
		g := New()
		g.BindPath(path, "global")
		g.enterScope(&Scope{}, false)
		if g.HasBindingPath(path) {
			t.Fatal("HasBindingPath reported a global materialization hidden from the active scope")
		}
	})

	t.Run("shared lazy binding is visible", func(t *testing.T) {
		g := New()
		g.BindLazyPath(path, func() (string, diag.Diagnostics) { return "lazy", nil })
		g.enterScope(&Scope{}, false)
		if !g.HasBindingPath(path) {
			t.Fatal("HasBindingPath missed a shared lazy binding")
		}
	})
}

// File-wide import aliases must not collide with scope-local variables.
func TestScopeVarAndImportAliasDoNotCollide(t *testing.T) {
	w := newScopeWorld(t)
	g := w.g
	pkg := typecheckScopePkg(t, "example.com/conn", "package conn\n\ntype Pool struct{}\n")
	pool := types.NewPointer(pkg.Scope().Lookup("Pool").Type())
	g.BindLazy(pool, "", func() (string, diag.Diagnostics) {
		v := g.Var("conn")
		g.Node(&Call{Base: Base{Phase: PhaseWire}, Var: v, Fn: g.Import("example.com/conn") + ".NewPool"})
		return v, nil
	})
	sp := g.AddScope("buildPool", token.Position{}, ScopeRoot{Type: pool})
	g.AddCommandFunc(CommandFunc{Name: "pool", Fn: "app.Pool", Scope: sp})
	src := renderScopes(t, g)
	if !strings.Contains(src, "conn2 \"example.com/conn\"") {
		t.Fatalf("import alias should rename around the scope var:\n%s", src)
	}
	if !strings.Contains(src, "conn := conn2.NewPool()") {
		t.Fatalf("scope var should keep its name with the renamed alias:\n%s", src)
	}
}

func TestScopeReservesRenderAliases(t *testing.T) {
	g := New()
	s := g.AddScope("build", token.Position{})
	g.enterScope(s, false)

	tests := []struct {
		path string
		name string
	}{
		{"os", "os"},
		{"fmt", "fmt"},
		{"errors", "errors"},
		{"os/signal", "signal"},
		{"syscall", "syscall"},
		{"github.com/gofabrik/fabrik/cli", "cli"},
	}
	for _, tc := range tests {
		if got := g.Var(tc.name); got != tc.name+"2" {
			t.Fatalf("scope variable %q = %q, want %q", tc.name, got, tc.name+"2")
		}
	}
	g.scope = nil
	for _, tc := range tests {
		if got := g.Import(tc.path); got != tc.name {
			t.Errorf("late import %q alias = %q, want stable alias %q", tc.path, got, tc.name)
		}
	}
}

func TestScopeUnwindFollowsEmissionOrder(t *testing.T) {
	w := newScopeWorld(t)
	g := w.g
	pkg := typecheckScopePkg(t, "example.com/ind", "package ind\n\ntype X struct{}\n\ntype Y struct{}\n\ntype C struct{}\n")
	xT := types.NewPointer(pkg.Scope().Lookup("X").Type())
	yT := types.NewPointer(pkg.Scope().Lookup("Y").Type())
	cT := types.NewPointer(pkg.Scope().Lookup("C").Type())
	// Layout emits Y before X despite insertion order.
	g.BindLazy(xT, "", func() (string, diag.Diagnostics) {
		v := g.Var("x")
		g.Node(&Call{Base: Base{Phase: PhaseWire, Origin: Origin{Pos: token.Position{Filename: "ind.go", Line: 30}}},
			Var: v, Fn: g.Import("example.com/ind") + ".NewX", Err: ErrReturn, Cleanup: g.Var(v + "Close")})
		return v, nil
	})
	g.BindLazy(yT, "", func() (string, diag.Diagnostics) {
		v := g.Var("y")
		g.Node(&Call{Base: Base{Phase: PhaseWire, Origin: Origin{Pos: token.Position{Filename: "ind.go", Line: 10}}},
			Var: v, Fn: g.Import("example.com/ind") + ".NewY", Err: ErrReturn, Cleanup: g.Var(v + "Close")})
		return v, nil
	})
	g.BindLazy(cT, "", func() (string, diag.Diagnostics) {
		xv, _, _ := g.Instance(xT, "")
		yv, _, _ := g.Instance(yT, "")
		v := g.Var("c")
		g.Node(&Call{Base: Base{Phase: PhaseWire, Origin: Origin{Pos: token.Position{Filename: "ind.go", Line: 40}}},
			Var: v, Fn: g.Import("example.com/ind") + ".NewC", Args: []string{xv, yv}, Err: ErrReturn})
		return v, nil
	})
	c1 := g.AddScope("buildC", token.Position{}, ScopeRoot{Type: cT})
	g.AddCommandFunc(CommandFunc{Name: "one", Fn: "app.One", Scope: c1})
	c2 := g.AddScope("buildC2", token.Position{}, ScopeRoot{Type: cT})
	g.AddCommandFunc(CommandFunc{Name: "two", Fn: "app.Two", Scope: c2})
	src := renderScopes(t, g)

	yPos := strings.Index(src, "y, yClose, err := ind.NewY()")
	xPos := strings.Index(src, "x, xClose, err := ind.NewX()")
	if yPos < 0 || xPos < 0 || yPos > xPos {
		t.Fatalf("layout should emit y before x:\n%s", src)
	}
	shape := `	var yClose, xClose func() error
	cleanup := func() error {
		var errs []error
		if xClose != nil {
			errs = append(errs, xClose())
		}
		if yClose != nil {
			errs = append(errs, yClose())
		}
		return errors.Join(errs...)
	}
	unwind := func(err error) error {
		return errors.Join(err, cleanup())
	}`
	if !strings.Contains(src, shape) {
		t.Fatalf("cleanup must release in reverse emission order (x before y):\n%s", src)
	}
}

func TestCleanupWithoutErrorSitesOmitsUnwind(t *testing.T) {
	w := newScopeWorld(t)
	g := w.g
	pkg := typecheckScopePkg(t, "example.com/pure", "package pure\n\ntype P struct{}\n")
	pT := types.NewPointer(pkg.Scope().Lookup("P").Type())
	g.BindLazy(pT, "", func() (string, diag.Diagnostics) {
		v := g.Var("p")
		g.Node(&Call{Base: Base{Phase: PhaseWire}, Var: v, Fn: g.Import("example.com/pure") + ".New", Cleanup: g.Var(v + "Close"), ErrsPkg: g.Import("errors")})
		return v, nil
	})
	p1 := g.AddScope("buildP", token.Position{}, ScopeRoot{Type: pT})
	g.AddCommandFunc(CommandFunc{Name: "one", Fn: "app.One", Scope: p1})
	p2 := g.AddScope("buildP2", token.Position{}, ScopeRoot{Type: pT})
	g.AddCommandFunc(CommandFunc{Name: "two", Fn: "app.Two", Scope: p2})
	src := renderScopes(t, g)
	if strings.Contains(src, "unwind") {
		t.Fatalf("no error sites, unwind must not be declared:\n%s", src)
	}
	if !strings.Contains(src, "p, pClose := pure.New()") || !strings.Contains(src, "err = errors.Join(err, pClose())") {
		t.Fatalf("cleanup must assign its slot and defer the join:\n%s", src)
	}
}

func TestRenderRejectsNestedCleanup(t *testing.T) {
	g := New()
	g.SetDirective("provider:select")
	g.Node(&Select{Var: "v", Iface: "web.I", KeyExpr: "k", FmtPkg: "fmt",
		Cases: []Case{{Value: "x", Result: Call{Var: "vx", Fn: "mk", Err: ErrReturn, Cleanup: "vxClose"}}}})
	if _, err := g.Render(); err == nil || !strings.Contains(err.Error(), "cleanup") {
		t.Fatalf("Render error = %v, want nested-cleanup rejection", err)
	}
}

func TestScopeMultiCleanupReverseOrder(t *testing.T) {
	w := newScopeWorld(t)
	g := w.g
	pkg := typecheckScopePkg(t, "example.com/two", "package two\n\ntype A struct{}\n\ntype B struct{}\n\ntype C struct{}\n")
	a := types.NewPointer(pkg.Scope().Lookup("A").Type())
	bT := types.NewPointer(pkg.Scope().Lookup("B").Type())
	cT := types.NewPointer(pkg.Scope().Lookup("C").Type())
	g.BindLazy(a, "", func() (string, diag.Diagnostics) {
		v := g.Var("a")
		g.Node(&Call{Base: Base{Phase: PhaseWire}, Var: v, Fn: g.Import("example.com/two") + ".NewA", Err: ErrReturn, Cleanup: g.Var(v + "Close")})
		return v, nil
	})
	g.BindLazy(bT, "", func() (string, diag.Diagnostics) {
		av, ds, _ := g.Instance(a, "")
		v := g.Var("b")
		g.Node(&Call{Base: Base{Phase: PhaseWire}, Var: v, Fn: g.Import("example.com/two") + ".NewB", Args: []string{av}, Err: ErrReturn, Cleanup: g.Var(v + "Close")})
		return v, ds
	})
	g.BindLazy(cT, "", func() (string, diag.Diagnostics) {
		bv, ds, _ := g.Instance(bT, "")
		v := g.Var("c")
		g.Node(&Call{Base: Base{Phase: PhaseWire}, Var: v, Fn: g.Import("example.com/two") + ".NewC", Args: []string{bv}, Err: ErrReturn})
		return v, ds
	})
	c1 := g.AddScope("buildC", token.Position{}, ScopeRoot{Type: cT})
	g.AddCommandFunc(CommandFunc{Name: "one", Fn: "app.One", Scope: c1})
	c2 := g.AddScope("buildC2", token.Position{}, ScopeRoot{Type: cT})
	g.AddCommandFunc(CommandFunc{Name: "two", Fn: "app.Two", Scope: c2})
	src := renderScopes(t, g)

	composed := `	var aClose, bClose func() error
	cleanup := func() error {
		var errs []error
		if bClose != nil {
			errs = append(errs, bClose())
		}
		if aClose != nil {
			errs = append(errs, aClose())
		}
		return errors.Join(errs...)
	}
	unwind := func(err error) error {
		return errors.Join(err, cleanup())
	}`
	if !strings.Contains(src, composed) {
		t.Fatalf("cleanup should pre-declare slots and release b then a:\n%s", src)
	}
	collapsed := `	c, err := two.NewC(b)
	if err != nil {
		return nil, nil, unwind(err)
	}`
	if !strings.Contains(src, collapsed) {
		t.Fatalf("error site should collapse to unwind(err):\n%s", src)
	}
}

func TestValidationPassDeterministicDiagnostics(t *testing.T) {
	order := func() []string {
		w := newScopeWorld(t)
		w.failAt = "store"
		pkg := typecheckScopePkg(t, "example.com/other", "package other\n\ntype T struct{}\n")
		oT := types.NewPointer(pkg.Scope().Lookup("T").Type())
		g := w.g
		g.BindLazy(oT, "", func() (string, diag.Diagnostics) {
			var ds diag.Diagnostics
			ds.Error(token.Position{Filename: "other.go", Line: 2, Column: 1}, "other is broken", "")
			return "", ds
		})
		ds := w.g.RunValidationPass()
		var msgs []string
		for _, d := range ds {
			msgs = append(msgs, d.Message)
		}
		return msgs
	}
	first, second := order(), order()
	if strings.Join(first, "|") != strings.Join(second, "|") {
		t.Fatalf("validation diagnostics order unstable:\n%v\n%v", first, second)
	}
	if len(first) < 2 {
		t.Fatalf("diagnostics = %v, want both broken providers reported", first)
	}
}

// A path binding materializes once; the demand graph shares it and each
// demanding flow re-emits the node.
func TestScopePathBindings(t *testing.T) {
	w := newScopeWorld(t)
	g := w.g
	const mgrPath = "*example.com/mgr.Manager"
	builds := 0
	g.BindLazyPath(mgrPath, func() (string, diag.Diagnostics) {
		builds++
		v := g.Var("m")
		g.Node(&Call{Base: Base{Phase: PhaseWire}, Var: v, Fn: g.Import("example.com/mgr") + ".New", Err: ErrReturn})
		g.BindPath(mgrPath, v)
		if again, _, ok := g.InstancePath(mgrPath); !ok || again != v {
			t.Fatalf("published path not visible during build: %q ok=%v", again, ok)
		}
		return v, nil
	})
	pkg := typecheckScopePkg(t, "example.com/user", "package user\n\ntype U struct{}\n")
	uT := types.NewPointer(pkg.Scope().Lookup("U").Type())
	g.BindLazy(uT, "", func() (string, diag.Diagnostics) {
		mgr, ds, ok := g.InstancePath(mgrPath)
		if !ok {
			return "", ds
		}
		again, _, _ := g.InstancePath(mgrPath)
		if again != mgr {
			t.Fatalf("scoped path cache miss: %q vs %q", again, mgr)
		}
		v := g.Var("u")
		g.Node(&Call{Base: Base{Phase: PhaseWire}, Var: v, Fn: g.Import("example.com/user") + ".New", Args: []string{mgr}, Err: ErrReturn})
		return v, ds
	})
	if expr, _, ok := g.InstancePath(mgrPath); !ok || expr == "" {
		t.Fatalf("default materialization failed")
	}
	if builds != 1 {
		t.Fatalf("default flow built %d times, want 1", builds)
	}
	u1 := g.AddScope("buildU1", token.Position{}, ScopeRoot{Type: uT})
	g.AddCommandFunc(CommandFunc{Name: "u1", Fn: "app.U1", Scope: u1})
	u2 := g.AddScope("buildU2", token.Position{}, ScopeRoot{Type: uT})
	g.AddCommandFunc(CommandFunc{Name: "u2", Fn: "app.U2", Scope: u2})
	src := renderScopes(t, g)
	// One materialization is shared through the demand graph; each
	// demanding flow re-emits the node.
	if builds != 1 {
		t.Fatalf("manager built %d times, want one shared materialization", builds)
	}
	if got := strings.Count(src, "mgr.New()"); got != 2 {
		t.Fatalf("manager constructed %d times in output, want once per command flow:\n%s", got, src)
	}
}

func TestValidationPassLeavesIdentifiersFree(t *testing.T) {
	w := newScopeWorld(t)
	if ds := w.g.RunValidationPass(); ds.HasFatal() {
		t.Fatalf("validation pass: %v", ds)
	}
	if got := w.g.Var("conn"); got != "conn" {
		t.Fatalf("Var after validation = %q, want conn", got)
	}
}
