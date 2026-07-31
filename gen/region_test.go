package gen

import (
	"bytes"
	"go/token"
	"go/types"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/diag"
)

// regionWorld drives the generator the way the engine does in fragment
// mode: lazy binds, command scopes, WalkFlows, PlanFragments, Render.
type regionWorld struct {
	g     *Gen
	store types.Type
	cache types.Type
}

func newRegionWorld(t *testing.T, storeName string) *regionWorld {
	t.Helper()
	pkg := typecheckScopePkg(t, "example.com/app", `package app

type Store struct{}

type Cache struct{}

type Server struct{}
`)
	w := &regionWorld{
		g:     New(),
		store: types.NewPointer(pkg.Scope().Lookup("Store").Type()),
		cache: types.NewPointer(pkg.Scope().Lookup("Cache").Type()),
	}
	w.g.SetModule("demo")
	w.g.FragmentMode()
	g := w.g
	g.BindLazy(w.store, storeName, func() (string, diag.Diagnostics) {
		v := g.Var("conn")
		g.Node(&Call{
			Base: Base{Phase: PhaseWire},
			Var:  v,
			Fn:   g.Import("example.com/app") + ".NewStore",
			Args: nil,
			Err:  ErrReturn,
			Type: w.store,
		})
		return v, nil
	})
	g.BindLazy(w.cache, "", func() (string, diag.Diagnostics) {
		store, ds, ok := g.Instance(w.store, storeName)
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
			Type: w.cache,
		})
		return v, ds
	})
	return w
}

func (w *regionWorld) addCommand(name string, roots ...ScopeRoot) {
	s := w.g.AddScope("build"+upperFirst(name), token.Position{}, roots...)
	w.g.AddCommandFunc(CommandFunc{
		Name:  name,
		Fn:    w.g.Import("example.com/app") + ".Run" + upperFirst(name),
		Scope: s,
	})
}

func renderRegions(t *testing.T, g *Gen) string {
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

func TestNamedProviderRegionExtractsOnce(t *testing.T) {
	w := newRegionWorld(t, "db")
	w.addCommand("alpha", ScopeRoot{Type: w.cache})
	w.addCommand("beta", ScopeRoot{Type: w.cache})
	src := renderRegions(t, w.g)

	if got := strings.Count(src, "app.NewStore()"); got != 1 {
		t.Fatalf("store constructed %d times, want one shared region:\n%s", got, src)
	}
	if !strings.Contains(src, "func buildDb() (*app.Cache, error)") {
		t.Fatalf("missing region function named for the provider:\n%s", src)
	}
	if got := strings.Count(src, "cache, err := buildDb()"); got != 2 {
		t.Fatalf("region called %d times, want once per command:\n%s", got, src)
	}
}

func TestSharedIdentifierRootExtractsAndNames(t *testing.T) {
	w := newRegionWorld(t, "")
	w.addCommand("alpha", ScopeRoot{Type: w.cache})
	w.addCommand("beta", ScopeRoot{Type: w.cache})
	src := renderRegions(t, w.g)

	// The shared chain ends in a scope root; the root anchors the
	// region, orders first, and names it by its result type.
	if !strings.Contains(src, "func buildCache() (*app.Cache, error)") {
		t.Fatalf("identifier scope root must anchor and name the region:\n%s", src)
	}
	if got := strings.Count(src, "app.NewStore()"); got != 1 {
		t.Fatalf("store constructed %d times, want one shared region:\n%s", got, src)
	}
}

func TestUnanchoredSharedNodesInlineDuplicated(t *testing.T) {
	w := newRegionWorld(t, "")
	g := w.g
	pkg := typecheckScopePkg(t, "example.com/wrap", `package wrap

type A struct{}

type B struct{}
`)
	wrapA := types.NewPointer(pkg.Scope().Lookup("A").Type())
	wrapB := types.NewPointer(pkg.Scope().Lookup("B").Type())
	for name, tt := range map[string]types.Type{"A": wrapA, "B": wrapB} {
		name, tt := name, tt
		g.BindLazy(tt, "", func() (string, diag.Diagnostics) {
			cache, ds, ok := g.Instance(w.cache, "")
			if !ok {
				return "", ds
			}
			v := g.Var("wrap" + name)
			g.Node(&Call{Base: Base{Phase: PhaseWire}, Var: v,
				Fn: g.Import("example.com/wrap") + ".New" + name, Args: []string{cache},
				Err: ErrReturn, Type: tt})
			return v, ds
		})
	}
	w.addCommand("alpha", ScopeRoot{Type: wrapA})
	w.addCommand("beta", ScopeRoot{Type: wrapB})
	src := renderRegions(t, w.g)

	// conn and cache are shared, but no member is a root, hook, named
	// provider, singleton, or prelude: nothing extracts.
	if got := strings.Count(src, "app.NewStore()"); got != 2 {
		t.Fatalf("store constructed %d times, want an inline copy per command:\n%s", got, src)
	}
	if strings.Contains(src, "func build") {
		t.Fatalf("unanchored nodes must not extract a build function:\n%s", src)
	}
}

func TestDemandPropagationUnifiesChains(t *testing.T) {
	pkg := typecheckScopePkg(t, "example.com/mix", `package mix

type P struct{}

type X struct{}

type Q struct{}
`)
	g := New()
	g.SetModule("demo")
	g.FragmentMode()
	tOf := func(name string) types.Type {
		return types.NewPointer(pkg.Scope().Lookup(name).Type())
	}
	p, x, q := tOf("P"), tOf("X"), tOf("Q")
	g.BindLazy(p, "seed", func() (string, diag.Diagnostics) {
		v := g.Var("p")
		g.Node(&Call{Base: Base{Phase: PhaseWire}, Var: v,
			Fn: g.Import("example.com/mix") + ".NewP", Err: ErrReturn, Type: p})
		return v, nil
	})
	g.BindLazy(x, "", func() (string, diag.Diagnostics) {
		pv, ds, ok := g.Instance(p, "seed")
		if !ok {
			return "", ds
		}
		v := g.Var("x")
		g.Node(&Call{Base: Base{Phase: PhaseWire}, Var: v,
			Fn: g.Import("example.com/mix") + ".NewX", Args: []string{pv},
			Err: ErrReturn, Type: x})
		return v, ds
	})
	g.BindLazy(q, "", func() (string, diag.Diagnostics) {
		xv, ds, ok := g.Instance(x, "")
		if !ok {
			return "", ds
		}
		v := g.Var("q")
		g.Node(&Call{Base: Base{Phase: PhaseWire}, Var: v,
			Fn: g.Import("example.com/mix") + ".NewQ", Args: []string{xv},
			Err: ErrReturn, Type: q})
		return v, ds
	})
	addCmd := func(name string, roots ...ScopeRoot) {
		s := g.AddScope("build"+upperFirst(name), token.Position{}, roots...)
		g.AddCommandFunc(CommandFunc{Name: name,
			Fn: g.Import("example.com/mix") + ".Run" + upperFirst(name), Scope: s})
	}
	// x's builder consumes p, so p inherits x's flows through demand
	// edges: p and x share one signature and extract together; q stays
	// a single-node inline, and gamma discards the p it does not use.
	addCmd("alpha", ScopeRoot{Type: p, Name: "seed"}, ScopeRoot{Type: q})
	addCmd("beta", ScopeRoot{Type: p, Name: "seed"}, ScopeRoot{Type: q})
	addCmd("gamma", ScopeRoot{Type: x})
	src := renderRegions(t, g)

	if !strings.Contains(src, "func buildSeed() (*mix.P, *mix.X, error)") {
		t.Fatalf("demand propagation must unify the p+x chain:\n%s", src)
	}
	if got := strings.Count(src, "mix.NewQ(x)"); got != 2 {
		t.Fatalf("q constructed %d times, want inline copies:\n%s", got, src)
	}
	if !strings.Contains(src, "p, x, err := buildSeed()") {
		t.Fatalf("alpha and beta must bind both region results:\n%s", src)
	}
	if !strings.Contains(src, "_, x, err := buildSeed()") {
		t.Fatalf("gamma must discard the unused live-out:\n%s", src)
	}
}

func TestAllDiscardedRegionCallAssigns(t *testing.T) {
	// A flow that consumes none of a region's results still calls it
	// for its effects; a short declaration would have no new name.
	g := New()
	g.SetModule("demo")
	g.FragmentMode()
	reg := &region{
		usage:    []string{"buildAlpha", "buildBeta"},
		usageSet: map[string]bool{"buildAlpha": true, "buildBeta": true},
		fn:       "buildThing",
		liveOuts: []string{"v"},
	}
	g.fragPlan = &regionPlan{steps: map[string][]planStep{"buildAlpha": {{region: reg}}}}
	g.regionConsumers = map[string][]regionConsumer{"v": {{flow: "buildBeta", reg: nil}}}
	s := &Scope{fn: "buildAlpha"}
	var b bytes.Buffer
	g.writeRegionBody(&b, CommandFunc{Fn: "app.RunAlpha", Scope: s})
	out := b.String()
	if !strings.Contains(out, "_, err = buildThing()") {
		t.Fatalf("all-discarded call must assign, not declare:\n%s", out)
	}
}

func TestSingleNodeSharedProviderInlines(t *testing.T) {
	w := newRegionWorld(t, "db")
	w.addCommand("alpha", ScopeRoot{Type: w.store, Name: "db"})
	w.addCommand("beta", ScopeRoot{Type: w.store, Name: "db"})
	src := renderRegions(t, w.g)

	if got := strings.Count(src, "app.NewStore()"); got != 2 {
		t.Fatalf("store constructed %d times, want an inline copy per command:\n%s", got, src)
	}
	if strings.Contains(src, "func build") {
		t.Fatalf("a single shared node must not extract:\n%s", src)
	}
}

func TestFlowUniqueWorkStaysInline(t *testing.T) {
	w := newRegionWorld(t, "db")
	w.addCommand("alpha", ScopeRoot{Type: w.cache})
	w.addCommand("beta", ScopeRoot{Type: w.store, Name: "db"})
	src := renderRegions(t, w.g)

	// Store is shared but cache is alpha-only; only the store could
	// extract and it is one node, so everything inlines.
	if got := strings.Count(src, "app.NewCache(conn)"); got != 1 {
		t.Fatalf("cache constructed %d times, want inline in alpha only:\n%s", got, src)
	}
	if strings.Contains(src, "func build") {
		t.Fatalf("no region expected:\n%s", src)
	}
}

func TestHookRegionTakesCalleeName(t *testing.T) {
	w := newRegionWorld(t, "db")
	g := w.g
	g.ScopePrologue(func() diag.Diagnostics {
		cfg := g.SingletonIn(PhaseConfig, "logcfg", "logCfg",
			g.Import("example.com/app")+".LoadLogConfig()")
		if !g.HasBinding(w.cache, "hooked") {
			g.Bind(w.cache, "hooked", cfg)
			g.Node(&Call{
				Base: Base{Phase: PhaseSetup},
				Fn:   g.Import("example.com/app") + ".InitLog",
				Args: []string{g.Context(), cfg},
				Err:  ErrInline,
			})
		}
		return nil
	})
	g.DeclareVarType("logCfg", "string", `""`)
	w.addCommand("alpha", ScopeRoot{Type: w.cache})
	w.addCommand("beta", ScopeRoot{Type: w.cache})
	src := renderRegions(t, w.g)

	if !strings.Contains(src, "func initLog(ctx context.Context) error") {
		t.Fatalf("hook region must take the hook callee's name:\n%s", src)
	}
	if got := strings.Count(src, "app.InitLog(ctx, logCfg)"); got != 1 {
		t.Fatalf("hook emitted %d times, want once inside its region:\n%s", got, src)
	}
	if got := strings.Count(src, "if err := initLog(ctx); err != nil"); got != 2 {
		t.Fatalf("hook region called %d times, want once per command:\n%s", got, src)
	}
	// Hooks run before providers in each command body.
	for _, body := range strings.SplitAfter(src, "Run: func") {
		hook := strings.Index(body, "initLog(ctx)")
		prov := strings.Index(body, "buildDb()")
		if hook >= 0 && prov >= 0 && prov < hook {
			t.Fatalf("providers precede the hook barrier:\n%s", src)
		}
	}
}

func TestSharedCompoundRootAssemblesInRegion(t *testing.T) {
	w := newRegionWorld(t, "db")
	g := w.g
	pkg := typecheckScopePkg(t, "example.com/srv", `package srv

type Server struct{}
`)
	server := types.NewPointer(pkg.Scope().Lookup("Server").Type())
	g.BindLazy(server, "", func() (string, diag.Diagnostics) {
		cache, ds, ok := g.Instance(w.cache, "")
		if !ok {
			return "", ds
		}
		return g.Import("example.com/srv") + ".New(" + cache + ")", ds
	})
	w.addCommand("alpha", ScopeRoot{Type: server})
	w.addCommand("beta", ScopeRoot{Type: server})
	src := renderRegions(t, w.g)

	if !strings.Contains(src, "func buildServer() (*srv.Server, error)") {
		t.Fatalf("assembled root must name and shape the region:\n%s", src)
	}
	if !strings.Contains(src, "return srv.New(cache), nil") {
		t.Fatalf("region must return the assembled value:\n%s", src)
	}
	if got := strings.Count(src, "server, err := buildServer()"); got != 2 {
		t.Fatalf("callers bind the assembled value, got %d call sites:\n%s", got, src)
	}
	if strings.Count(src, "srv.New(cache)") != 1 {
		t.Fatalf("assembly must happen once, inside the region:\n%s", src)
	}
}

func TestWideRegionFallsBackToInline(t *testing.T) {
	pkg := typecheckScopePkg(t, "example.com/wide", `package wide

type A struct{}

type B struct{}

type C struct{}

type D struct{}

type E struct{}
`)
	g := New()
	g.SetModule("demo")
	g.FragmentMode()
	tOf := func(name string) types.Type {
		return types.NewPointer(pkg.Scope().Lookup(name).Type())
	}
	base := tOf("A")
	g.BindLazy(base, "hub", func() (string, diag.Diagnostics) {
		v := g.Var("a")
		g.Node(&Call{Base: Base{Phase: PhaseWire}, Var: v,
			Fn: g.Import("example.com/wide") + ".NewA", Err: ErrReturn, Type: base})
		return v, nil
	})
	for _, name := range []string{"B", "C", "D", "E"} {
		name := name
		tt := tOf(name)
		g.BindLazy(tt, "", func() (string, diag.Diagnostics) {
			a, ds, ok := g.Instance(base, "hub")
			if !ok {
				return "", ds
			}
			v := g.Var(strings.ToLower(name))
			g.Node(&Call{Base: Base{Phase: PhaseWire}, Var: v,
				Fn: g.Import("example.com/wide") + ".New" + name, Args: []string{a},
				Err: ErrReturn, Type: tt})
			return v, ds
		})
	}
	roots := []ScopeRoot{{Type: tOf("B")}, {Type: tOf("C")}, {Type: tOf("D")}, {Type: tOf("E")}}
	s1 := g.AddScope("buildAlpha", token.Position{}, roots...)
	g.AddCommandFunc(CommandFunc{Name: "alpha", Fn: g.Import("example.com/wide") + ".RunAlpha", Scope: s1})
	s2 := g.AddScope("buildBeta", token.Position{}, roots...)
	g.AddCommandFunc(CommandFunc{Name: "beta", Fn: g.Import("example.com/wide") + ".RunBeta", Scope: s2})
	src := renderRegions(t, g)

	// Four live-outs exceed the width guard; the shared chain inlines.
	if strings.Contains(src, "func build") {
		t.Fatalf("wide region must not extract:\n%s", src)
	}
	if got := strings.Count(src, "wide.NewA()"); got != 2 {
		t.Fatalf("hub constructed %d times, want an inline copy per command:\n%s", got, src)
	}
}

func TestPathBindPositionAnchorsDiagnostics(t *testing.T) {
	// Positionless nodes emitted while materializing a path binding
	// inherit the registering annotation's position in diagnostics.
	pkg := typecheckScopePkg(t, "example.com/app", `package app

type Adapter struct{}
`)
	adapter := pkg.Scope().Lookup("Adapter").Type()
	g := New()
	g.SetModule("demo")
	g.FragmentMode()
	bindPos := token.Position{Filename: "web.go", Line: 7, Column: 1}
	g.BindLazyPathAt("example.com/app.Adapter", bindPos, func() (string, diag.Diagnostics) {
		g.Node(&Assign{Base: Base{Phase: PhaseWire}, Var: "adapter", Expr: "app.New()"})
		g.Node(&Assign{Base: Base{Phase: PhaseWire}, Var: "adapter", Expr: "app.New()"})
		return "adapter", nil
	})
	g.BindLazyPathAt("example.com/app.Picker", bindPos, func() (string, diag.Diagnostics) {
		g.Node(&Select{
			Base:    Base{Phase: PhaseWire},
			Var:     "picker",
			Iface:   "app.Adapter",
			KeyExpr: `"fixed"`,
			Cases: []Case{{
				Value: "fixed",
				Body: []Node{
					&Assign{Base: Base{Phase: PhaseWire}, Var: "inner", Expr: "app.New()"},
					&Assign{Base: Base{Phase: PhaseWire}, Var: "inner", Expr: "app.New()"},
				},
				Result: Call{Base: Base{Phase: PhaseWire}, Var: "picker", Fn: "app.Pick",
					Args: []string{"inner"}, Err: ErrNone},
			}},
		})
		return "picker", nil
	})
	s := g.AddScope("buildServe", token.Position{Filename: "cli.go", Line: 3}, ScopeRoot{Type: adapter})
	g.AddCommandFunc(CommandFunc{Name: "serve", Fn: "app.RunServe", Scope: s})
	if ds := g.WalkFlows(); ds.HasFatal() {
		t.Fatalf("WalkFlows: %v", ds)
	}
	if _, pds, ok := g.InstancePath("example.com/app.Picker"); !ok || pds.HasFatal() {
		t.Fatalf("InstancePath: %v", pds)
	}
	ds := g.ValidateGraph()
	found, nested := false, false
	for _, d := range ds {
		if d.Pos == bindPos && strings.Contains(d.Message, `"adapter" defined twice`) {
			found = true
		}
		if d.Pos == bindPos && strings.Contains(d.Message, `"inner" defined twice in one case`) {
			nested = true
		}
	}
	if !found {
		t.Fatalf("duplicate-define diagnostic must carry the path bind's position, got %v", ds)
	}
	if !nested {
		t.Fatalf("a nested select child's diagnostic must inherit the bind position, got %v", ds)
	}
}
