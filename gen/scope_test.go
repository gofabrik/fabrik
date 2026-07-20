package gen

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
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
	if ds := g.MaterializeScopes(); ds.HasFatal() {
		t.Fatalf("MaterializeScopes: %v", ds)
	}
	out, err := g.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return string(out)
}

func TestScopeBuildsItsSubtree(t *testing.T) {
	w := newScopeWorld(t)
	w.g.AddScope("buildPing", token.Position{}, w.cache)
	src := renderScopes(t, w.g)

	want := `func buildPing(ctx context.Context) (*app.Cache, func(), error) {
	// Providers
	conn, connClose, err := app.NewStore(ctx)
	if err != nil {
		return nil, nil, err
	}
	cache, err := app.NewCache(conn)
	if err != nil {
		if connClose != nil {
			connClose()
		}
		return nil, nil, err
	}

	cleanup := func() {
		if connClose != nil {
			connClose()
		}
	}
	return cache, cleanup, nil
}`
	if !strings.Contains(src, want) {
		t.Fatalf("buildPing shape mismatch.\n--- want ---\n%s\n--- got ---\n%s", want, src)
	}
}

func TestScopesConstructIndependently(t *testing.T) {
	w := newScopeWorld(t)
	w.g.AddScope("buildA", token.Position{}, w.store)
	w.g.AddScope("buildB", token.Position{}, w.store)
	src := renderScopes(t, w.g)

	if got := strings.Count(src, "app.NewStore(ctx)"); got != 2 {
		t.Fatalf("store constructed %d times, want once per scope:\n%s", got, src)
	}
	if strings.Contains(src, "conn2") {
		t.Fatalf("scope-local names leaked across scopes:\n%s", src)
	}
	if !strings.Contains(src, "func buildA(ctx context.Context) (*app.Store, func(), error)") ||
		!strings.Contains(src, "func buildB(ctx context.Context) (*app.Store, func(), error)") {
		t.Fatalf("missing scope functions:\n%s", src)
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
	g.AddScope("buildFlags", token.Position{}, flags)
	src := renderScopes(t, g)

	if !strings.Contains(src, "func buildFlags(ctx context.Context) (*flag.Flags, error) {") {
		t.Fatalf("cleanup-free scope should omit the cleanup slot:\n%s", src)
	}
	if strings.Contains(src, "buildFlags") && strings.Contains(src[strings.Index(src, "buildFlags"):], "cleanup :=") {
		t.Fatalf("cleanup composed in a cleanup-free scope:\n%s", src)
	}
}

func TestScopeContextBindsToParam(t *testing.T) {
	w := newScopeWorld(t)
	w.g.AddScope("buildPing", token.Position{}, w.store)
	src := renderScopes(t, w.g)

	if !strings.Contains(src, "app.NewStore(ctx)") {
		t.Fatalf("scoped Context() should bind to the build param:\n%s", src)
	}
	if strings.Contains(src, "context.Background()") {
		t.Fatalf("scope ctx leaked into run():\n%s", src)
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

func TestTransformReturns(t *testing.T) {
	in := []string{
		"v, err := build()",
		"return err",
		`return fmt.Errorf("no impl for %q", kind)`,
		"return nil",
	}
	got := transformReturns(in, "nil, ", []string{"aClose"})
	want := []string{
		"v, err := build()",
		"if aClose != nil {",
		"aClose()",
		"}",
		"return nil, err",
		"if aClose != nil {",
		"aClose()",
		"}",
		`return nil, fmt.Errorf("no impl for %q", kind)`,
		"return nil",
	}
	if len(got) != len(want) {
		t.Fatalf("lines = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d = %q, want %q\nall: %v", i, got[i], want[i], got)
		}
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
	g.AddScope("buildSrv", token.Position{}, srv)
	src := renderScopes(t, g)
	if strings.Contains(src, "func buildSrv") {
		t.Fatalf("nodeless scope must not emit a build function:\n%s", src)
	}
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
	g.AddScope("buildPool", token.Position{}, pool)
	src := renderScopes(t, g)
	if !strings.Contains(src, "conn2 \"example.com/conn\"") {
		t.Fatalf("import alias should rename around the scope var:\n%s", src)
	}
	if !strings.Contains(src, "conn := conn2.NewPool()") {
		t.Fatalf("scope var should keep its name with the renamed alias:\n%s", src)
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
	g.AddScope("buildC", token.Position{}, cT)
	src := renderScopes(t, g)

	unwind := `	c, err := two.NewC(b)
	if err != nil {
		if bClose != nil {
			bClose()
		}
		if aClose != nil {
			aClose()
		}
		return nil, nil, err
	}`
	if !strings.Contains(src, unwind) {
		t.Fatalf("unwind should release b then a:\n%s", src)
	}
	composed := `	cleanup := func() {
		if bClose != nil {
			bClose()
		}
		if aClose != nil {
			aClose()
		}
	}`
	if !strings.Contains(src, composed) {
		t.Fatalf("composed cleanup should release in reverse:\n%s", src)
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

// Path bindings cache independently in the default flow and each scope.
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
	// Scopes must not reuse a path expression cached by the default flow.
	if expr, _, ok := g.InstancePath(mgrPath); !ok || expr == "" {
		t.Fatalf("default materialization failed")
	}
	if builds != 1 {
		t.Fatalf("default flow built %d times, want 1", builds)
	}
	g.AddScope("buildU1", token.Position{}, uT)
	g.AddScope("buildU2", token.Position{}, uT)
	src := renderScopes(t, g)
	if builds != 3 {
		t.Fatalf("manager built %d times, want the default plus once per scope", builds)
	}
	if got := strings.Count(src, "mgr.New()"); got != 3 {
		t.Fatalf("manager constructed %d times in output, want 3:\n%s", got, src)
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
