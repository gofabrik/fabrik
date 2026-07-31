package gen

import (
	"bytes"
	"encoding/json"
	"go/token"
	"go/types"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/diag"
)

func graphWorld(t *testing.T) *Gen {
	t.Helper()
	pkg := typecheckScopePkg(t, "example.com/app", `package app

type Store struct{}

type Cache struct{}

type Greeter interface{ Greet() string }
`)
	store := types.NewPointer(pkg.Scope().Lookup("Store").Type())
	cache := types.NewPointer(pkg.Scope().Lookup("Cache").Type())
	greeter := pkg.Scope().Lookup("Greeter").Type()

	g := New()
	g.SetModule("example.com/app")
	g.SetSourceRoot("/work/app")
	g.SetDirective("provider")
	g.BindLazy(store, "", func() (string, diag.Diagnostics) {
		v := g.Var("conn")
		c := g.Var(v + "Close")
		g.Node(&Call{
			Base:    Base{Phase: PhaseWire, Origin: Origin{Directive: "provider", Pos: token.Position{Filename: "/work/app/shared/db.go", Line: 14}}},
			Var:     v,
			Fn:      g.Import("example.com/app") + ".NewStore",
			Args:    []string{g.Context()},
			Err:     ErrReturn,
			Type:    store,
			Cleanup: c,
			ErrsPkg: g.Import("errors"),
		})
		return v, nil
	})
	g.BindLazy(cache, "", func() (string, diag.Diagnostics) {
		conn, ds, _ := g.Instance(store, "")
		return g.Import("example.com/app") + ".NewCache(" + conn + ")", ds
	})

	s := g.AddScope("buildRun", token.Position{}, ScopeRoot{Type: cache})
	if ds := g.MaterializeScopes(); ds.HasFatal() {
		t.Fatalf("MaterializeScopes: %v", ds)
	}
	s.nodes = append(s.nodes,
		&Raw{
			Base:    Base{Phase: PhaseConfig, Origin: Origin{Directive: "config"}},
			Lines:   []string{`appEnv := "dev"`, "appOpts := []string{appEnv}"},
			Defines: []string{"appEnv", "appOpts"},
		},
		&Select{
			Base: Base{
				Phase:  PhaseWire,
				Origin: Origin{Directive: "provider:select", Pos: token.Position{Filename: "/work/app/web/greeter.go", Line: 11}},
				Label:  "app.Greeter, selected by greeter.kind",
			},
			Var: "greeter", Iface: "app.Greeter", KeyExpr: "appEnv", FmtPkg: g.Import("fmt"),
			Cases: []Case{{
				Value: "fancy",
				Result: Call{
					Base: Base{Origin: Origin{Directive: "provider", Pos: token.Position{Filename: "/work/app/web/greeter.go", Line: 29}}},
					Var:  "greeterFancy", Fn: "app.NewFancyGreeter", Args: []string{"conn"},
					Type: greeter, Err: ErrReturn,
				},
			}, {
				Value: "plain",
				Result: Call{
					Base: Base{Origin: Origin{Directive: "provider", Pos: token.Position{Filename: "/work/app/web/greeter.go", Line: 40}}},
					Fn:   "app.NewPlainGreeter", Args: []string{"conn"},
					Type: greeter,
				},
			}},
		},
		&Route{
			Base:   Base{Phase: PhaseRegister, Origin: Origin{Directive: "http:handle", Pos: token.Position{Filename: "/work/app/web/web.go", Line: 33}}},
			Router: "r", Kind: RouteMethod, Method: "GET", Pattern: "/", Handler: "appAPI.Greet",
		},
		&Call{
			Base: Base{Phase: PhaseRegister, Origin: Origin{Directive: "job", Pos: token.Position{Filename: "/work/app/web/visits.go", Line: 18}}},
			Fn:   "jobs.On[app.Visit]", Args: []string{"conn"}, Err: ErrInline,
		},
	)
	if _, err := g.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	return g
}

func flowByID(t *testing.T, gr *Graph, id string) GraphFlow {
	t.Helper()
	for _, f := range gr.Flows {
		if f.ID == id {
			return f
		}
	}
	t.Fatalf("no flow %q in %+v", id, gr.Flows)
	return GraphFlow{}
}

func nodeByID(t *testing.T, gr *Graph, id string) GraphNode {
	t.Helper()
	for _, n := range gr.Nodes {
		if n.ID == id {
			return n
		}
	}
	t.Fatalf("no node %q in %+v", id, gr.Nodes)
	return GraphNode{}
}

func hasEdge(gr *Graph, from, to string) bool {
	for _, e := range gr.Edges {
		if e.From == from && e.To == to {
			return true
		}
	}
	return false
}

func TestGraphFlowsCarryRootsAndBindings(t *testing.T) {
	gr := graphWorld(t).Graph()

	if gr.Version != 1 || gr.Module != "example.com/app" {
		t.Fatalf("envelope = %d, %q", gr.Version, gr.Module)
	}
	run := flowByID(t, gr, "run")
	if run.Fn != "buildRun" {
		t.Fatalf("flow fn = %q, want buildRun", run.Fn)
	}
	want := []GraphBinding{{
		ID:    "run/bind@0",
		Expr:  "app.NewCache(conn)",
		Types: []GraphType{{Display: "*app.Cache", Canonical: "*example.com/app.Cache"}},
	}}
	if !reflect.DeepEqual(run.Bindings, want) {
		t.Fatalf("bindings = %+v, want %+v", run.Bindings, want)
	}
	wantRoots := []GraphRoot{{
		ID:      "run/root@0",
		Type:    GraphType{Display: "*app.Cache", Canonical: "*example.com/app.Cache"},
		Binding: "run/bind@0",
	}}
	if !reflect.DeepEqual(run.Roots, wantRoots) {
		t.Fatalf("roots = %+v, want %+v", run.Roots, wantRoots)
	}
	if !hasEdge(gr, "run/bind@0", "run/conn") {
		t.Fatalf("binding edge missing: %+v", gr.Edges)
	}
	if hasEdge(gr, "run/root@0", "run/conn") {
		t.Fatalf("a root referencing a binding must not duplicate its edges: %+v", gr.Edges)
	}
}

func TestGraphRootExpressionCarriesItsOwnEdges(t *testing.T) {
	pkg := typecheckScopePkg(t, "example.com/app", `package app

type Store struct{}
`)
	store := types.NewPointer(pkg.Scope().Lookup("Store").Type())
	g := New()
	g.SetModule("example.com/app")
	g.SetDirective("provider")
	g.BindLazy(store, "", func() (string, diag.Diagnostics) {
		v := g.Var("conn")
		g.Node(&Call{Base: Base{Phase: PhaseWire}, Var: v, Fn: g.Import("example.com/app") + ".NewStore"})
		return v, nil
	})
	g.AddScope("buildMigrate", token.Position{}, ScopeRoot{Type: store})
	if ds := g.MaterializeScopes(); ds.HasFatal() {
		t.Fatalf("MaterializeScopes: %v", ds)
	}
	if _, err := g.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	gr := g.Graph()

	migrate := flowByID(t, gr, "migrate")
	if len(migrate.Bindings) != 0 {
		t.Fatalf("a bare variable binding must not become an entry: %+v", migrate.Bindings)
	}
	if len(migrate.Roots) != 1 || migrate.Roots[0].Expr != "conn" || migrate.Roots[0].Binding != "" {
		t.Fatalf("roots = %+v", migrate.Roots)
	}
	if !hasEdge(gr, "migrate/root@0", "migrate/conn") {
		t.Fatalf("root expression edge missing: %+v", gr.Edges)
	}
}

func TestGraphNodesDescribeEveryKind(t *testing.T) {
	gr := graphWorld(t).Graph()

	conn := nodeByID(t, gr, "run/conn")
	wantConn := GraphNode{
		ID: "run/conn", Flow: "run", Kind: "call", Defines: []string{"conn"},
		Fn: "app.NewStore", Type: "*app.Store",
		TypeCanonical: "*example.com/app.Store", Cleanup: "connClose",
		Directive: "provider", Pos: "shared/db.go:14",
	}
	if !reflect.DeepEqual(conn, wantConn) {
		t.Fatalf("call node = %+v, want %+v", conn, wantConn)
	}

	raw := nodeByID(t, gr, "run/appEnv")
	if raw.Kind != "raw" || !reflect.DeepEqual(raw.Defines, []string{"appEnv", "appOpts"}) || raw.Pos != "" {
		t.Fatalf("raw node = %+v", raw)
	}

	sel := nodeByID(t, gr, "run/greeter")
	if sel.Kind != "select" || sel.Type != "app.Greeter" || sel.Label == "" {
		t.Fatalf("select node = %+v", sel)
	}
	child := nodeByID(t, gr, "run/greeterFancy")
	if child.Parent != "run/greeter" || child.Case != "fancy" || child.Type != "app.Greeter" {
		t.Fatalf("select child = %+v", child)
	}
	if !hasEdge(gr, "run/greeterFancy", "run/conn") {
		t.Fatalf("select child keeps its own inputs: %+v", gr.Edges)
	}
	if hasEdge(gr, "run/greeter", "run/conn") {
		t.Fatalf("branch inputs must not aggregate onto the select: %+v", gr.Edges)
	}
	if !hasEdge(gr, "run/greeter", "run/appEnv") {
		t.Fatalf("select key edge missing: %+v", gr.Edges)
	}

	route := nodeByID(t, gr, "run/route@0")
	if route.Kind != "route" || route.Method != "GET" || route.Pattern != "/" || len(route.Defines) != 0 {
		t.Fatalf("route node = %+v", route)
	}
	// The ordinal counts every call of the flow, defining ones included.
	call := nodeByID(t, gr, "run/call@3")
	if call.Fn != "jobs.On[app.Visit]" || len(call.Defines) != 0 {
		t.Fatalf("var-less call node = %+v", call)
	}
}

func TestGraphDefaultFlow(t *testing.T) {
	g := New()
	g.SetModule("demo")
	g.SetDirective("provider")
	g.Node(&Call{Base: Base{Phase: PhaseWire}, Var: "db", Fn: "shared.NewDB"})
	g.Node(&StructLit{Base: Base{Phase: PhaseWire}, Var: "api", Type: "web.API", Fields: []Field{{Name: "DB", Expr: "db"}}})
	if _, err := g.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	gr := g.Graph()

	def := flowByID(t, gr, "default")
	if def.Fn != "run" || len(def.Roots) != 0 || len(def.Bindings) != 0 {
		t.Fatalf("default flow = %+v", def)
	}
	api := nodeByID(t, gr, "default/api")
	if api.Kind != "structLit" || api.Type != "*web.API" {
		t.Fatalf("structlit node = %+v", api)
	}
	if !hasEdge(gr, "default/api", "default/db") {
		t.Fatalf("edges = %+v", gr.Edges)
	}
}

func TestGraphConsumerArgumentTargetsMatchingBinding(t *testing.T) {
	// A matching node argument still targets the root-only cache binding.
	g := graphWorld(t)
	s := g.scopes[0]
	s.nodes = append(s.nodes, &Call{
		Base: Base{Phase: PhaseRegister, Origin: Origin{Directive: "job"}},
		Fn:   "app.Warm", Args: []string{"app.NewCache(conn)"}, Err: ErrInline,
	})
	if _, err := g.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	gr := g.Graph()
	if !hasEdge(gr, "run/call@4", "run/bind@0") {
		t.Fatalf("consumer argument must target the binding: %+v", gr.Edges)
	}
	if hasEdge(gr, "run/call@4", "run/conn") {
		t.Fatalf("a matched binding argument must not also emit free-identifier edges: %+v", gr.Edges)
	}
}

func TestGraphIsDeterministic(t *testing.T) {
	first, err := json.Marshal(graphWorld(t).Graph())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, err := json.Marshal(graphWorld(t).Graph())
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, again) {
			t.Fatalf("graph is not deterministic:\nfirst:\n%s\nagain:\n%s", first, again)
		}
	}
	gr := graphWorld(t).Graph()
	for i := 1; i < len(gr.Edges); i++ {
		prev, cur := gr.Edges[i-1], gr.Edges[i]
		if prev.From > cur.From || (prev.From == cur.From && prev.To > cur.To) {
			t.Fatalf("edges are not sorted: %+v", gr.Edges)
		}
	}
}

func TestGraphLeavesRenderedSourceUnchanged(t *testing.T) {
	g := graphWorld(t)
	before, err := g.Render()
	if err != nil {
		t.Fatal(err)
	}
	imports := len(g.imports)
	g.Graph()
	after, err := g.Render()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("Graph changed rendered source:\n--- before ---\n%s--- after ---\n%s", before, after)
	}
	if len(g.imports) != imports {
		t.Fatalf("Graph registered imports: %d, want %d", len(g.imports), imports)
	}
}

func TestTypeDisplayUsesRegisteredAliasesWithoutImporting(t *testing.T) {
	pkg := typecheckScopePkg(t, "example.com/app", `package app

type Store struct{}
`)
	other := typecheckScopePkg(t, "example.com/other/app", `package app

type Store struct{}
`)
	g := New()
	store := types.NewPointer(pkg.Scope().Lookup("Store").Type())
	otherStore := types.NewPointer(other.Scope().Lookup("Store").Type())

	if got := g.TypeDisplay(store); got != "*app.Store" {
		t.Fatalf("unregistered display = %q, want the package name fallback", got)
	}
	if len(g.imports) != 0 {
		t.Fatalf("typeDisplay registered an import: %v", g.imports)
	}
	g.Import("example.com/app")
	g.Import("example.com/other/app")
	if got := g.TypeDisplay(otherStore); got != "*app2.Store" {
		t.Fatalf("registered display = %q, want the Render alias", got)
	}
}

func TestGraphRenderings(t *testing.T) {
	gr := graphWorld(t).Graph()

	dot := gr.DOT()
	for _, want := range []string{
		"digraph fabrik {",
		"subgraph cluster_f1 {",
		`label="buildRun";`,
		`"run/conn" [label="conn\n*app.Store (cleanup)\nprovider  shared/db.go:14"];`,
		`"run/bind@0" -> "run/conn";`,
	} {
		if !strings.Contains(dot, want) {
			t.Errorf("DOT missing %q:\n%s", want, dot)
		}
	}

	mmd := gr.Mermaid()
	for _, want := range []string{
		"flowchart TB",
		`subgraph f1["buildRun"]`,
		`n3["conn<br/>*app.Store (cleanup)<br/>provider shared/db.go:14"]`,
		"n0 --> n3",
		"end",
	} {
		if !strings.Contains(mmd, want) {
			t.Errorf("mermaid missing %q:\n%s", want, mmd)
		}
	}
	if strings.Contains(dot, "/work/app") || strings.Contains(mmd, "/work/app") {
		t.Errorf("renderings leak absolute paths")
	}
	// Registration effects remain JSON-only.
	for _, absent := range []string{"route@0", "GET /", "jobs.On"} {
		if strings.Contains(dot, absent) || strings.Contains(mmd, absent) {
			t.Errorf("renderings must omit registration effects, found %q", absent)
		}
	}
	// Select constructors without errors use assignment but retain their edges.
	if !strings.Contains(dot, `"run/call@`) || !strings.Contains(dot, "app.NewPlainGreeter") {
		t.Errorf("non-error select implementation missing from DOT:\n%s", dot)
	}
}

func TestGraphNodesFollowEmissionOrder(t *testing.T) {
	g := New()
	g.SetModule("demo")
	g.SetDirective("provider")
	// Config emits first despite reverse insertion order.
	g.Node(&Call{Base: Base{Phase: PhaseWire, Origin: Origin{Directive: "provider"}}, Fn: "register"})
	g.Node(&Call{Base: Base{Phase: PhaseConfig, Origin: Origin{Directive: "config"}}, Fn: "loadEnv"})
	gr := g.Graph()
	if len(gr.Nodes) != 2 || gr.Nodes[0].Fn != "loadEnv" || gr.Nodes[1].Fn != "register" {
		t.Fatalf("nodes must follow emission order, got %+v", gr.Nodes)
	}
	if gr.Nodes[0].ID != "default/call@0" || gr.Nodes[1].ID != "default/call@1" {
		t.Fatalf("ordinals must follow emission order, got %q, %q", gr.Nodes[0].ID, gr.Nodes[1].ID)
	}
}

func TestGraphDefaultFlowBindingsAndConsumerTargeting(t *testing.T) {
	pkg := typecheckScopePkg(t, "example.com/dflt", "package dflt\n\ntype Server struct{}\n")
	srv := types.NewPointer(pkg.Scope().Lookup("Server").Type())
	g := New()
	g.SetModule("demo")
	g.SetDirective("http")
	g.Node(&Assign{Base: Base{Phase: PhaseWire, Origin: Origin{Directive: "http"}}, Var: "r", Expr: "router.New()"})
	g.Bind(srv, "", "dflt.NewServer(r)")
	g.Node(&Call{Base: Base{Phase: PhaseRegister, Origin: Origin{Directive: "http"}}, Fn: "serve", Args: []string{"dflt.NewServer(r)"}})
	gr := g.Graph()
	if len(gr.Flows) == 0 || gr.Flows[0].ID != "default" {
		t.Fatalf("default flow must always be exported, got %+v", gr.Flows)
	}
	binds := gr.Flows[0].Bindings
	if len(binds) != 1 || binds[0].Expr != "dflt.NewServer(r)" || binds[0].ID != "default/bind@0" {
		t.Fatalf("default flow bindings = %+v", binds)
	}
	wantEdges := []GraphEdge{
		{From: "default/bind@0", To: "default/r"},
		{From: "default/call@0", To: "default/bind@0"},
	}
	for _, want := range wantEdges {
		if !slices.Contains(gr.Edges, want) {
			t.Errorf("missing edge %+v in %+v", want, gr.Edges)
		}
	}
}

func TestGraphBindingAssociationsCarryNames(t *testing.T) {
	pkg := typecheckScopePkg(t, "example.com/dflt", "package dflt\n\ntype Server struct{}\n")
	srv := types.NewPointer(pkg.Scope().Lookup("Server").Type())
	g := New()
	g.SetModule("demo")
	g.SetDirective("http")
	g.Bind(srv, "replica", "dflt.NewServer(r)")
	g.Bind(srv, "primary", "dflt.NewServer(r)")
	gr := g.Graph()

	binds := gr.Flows[0].Bindings
	want := []GraphType{
		{Display: "*dflt.Server", Canonical: "*example.com/dflt.Server", Name: "primary"},
		{Display: "*dflt.Server", Canonical: "*example.com/dflt.Server", Name: "replica"},
	}
	if len(binds) != 1 || !reflect.DeepEqual(binds[0].Types, want) {
		t.Fatalf("types = %+v, want %+v", binds[0].Types, want)
	}
}

func TestGraphMultiNameBindingIsDeterministic(t *testing.T) {
	build := func(t *testing.T) *Gen {
		pkg := typecheckScopePkg(t, "example.com/multi", "package multi\n\ntype Server struct{}\n")
		srv := types.NewPointer(pkg.Scope().Lookup("Server").Type())
		g := New()
		g.SetModule("example.com/multi")
		g.SetDirective("provider")
		g.Bind(srv, "replica", "multi.NewServer(r)")
		g.Bind(srv, "primary", "multi.NewServer(r)")
		return g
	}
	first, err := json.Marshal(build(t).Graph())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, err := json.Marshal(build(t).Graph())
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, again) {
			t.Fatalf("graph is not deterministic:\nfirst:\n%s\nagain:\n%s", first, again)
		}
	}
}

func TestGraphRootCarriesName(t *testing.T) {
	pkg := typecheckScopePkg(t, "example.com/app", `package app

type Store struct{}
`)
	store := types.NewPointer(pkg.Scope().Lookup("Store").Type())
	g := New()
	g.SetModule("example.com/app")
	g.SetDirective("provider")
	g.BindLazy(store, "replica", func() (string, diag.Diagnostics) {
		v := g.Var("conn")
		g.Node(&Call{Base: Base{Phase: PhaseWire}, Var: v, Fn: g.Import("example.com/app") + ".NewStore"})
		return v, nil
	})
	g.AddScope("buildMigrate", token.Position{}, ScopeRoot{Type: store, Name: "replica"})
	if ds := g.MaterializeScopes(); ds.HasFatal() {
		t.Fatalf("MaterializeScopes: %v", ds)
	}
	if _, err := g.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	gr := g.Graph()

	migrate := flowByID(t, gr, "migrate")
	if len(migrate.Roots) != 1 || migrate.Roots[0].Name != "replica" {
		t.Fatalf("roots = %+v", migrate.Roots)
	}
}

func TestGraphNodeExposesBindingName(t *testing.T) {
	g := New()
	g.SetModule("demo")
	g.SetDirective("provider")
	g.Node(&Call{Base: Base{Phase: PhaseWire}, Var: "db", Fn: "shared.NewDB", BindingName: "replica"})
	before, err := g.Render()
	if err != nil {
		t.Fatal(err)
	}
	gr := g.Graph()
	node := nodeByID(t, gr, "default/db")
	if node.BindingName != "replica" {
		t.Fatalf("bindingName = %q, want replica", node.BindingName)
	}
	after, err := g.Render()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || strings.Contains(string(after), "replica") {
		t.Fatalf("BindingName must be graph-only metadata, got:\n%s", after)
	}
}

func TestGraphBindingOrdinalsFollowBindOrder(t *testing.T) {
	g := New()
	g.SetModule("demo")
	g.SetDirective("templates")
	// Reversed registration pins lexical ordering of paths and expressions.
	g.BindPath("zeta.Set", "makeZ()")
	g.BindPath("alpha.Set", "makeA()")
	gr := g.Graph()
	binds := gr.Flows[0].Bindings
	if len(binds) != 2 || binds[0].Expr != "makeZ()" || binds[1].Expr != "makeA()" {
		t.Fatalf("ordinals must follow first-bind order, got %+v", binds)
	}
}

func TestMermaidIDsAreCollisionSafe(t *testing.T) {
	g := New()
	g.SetModule("demo")
	g.SetDirective("provider")
	// Sequential IDs keep colliding sanitized names distinct and omit var-less nodes.
	g.Node(&Assign{Base: Base{Phase: PhaseWire, Origin: Origin{Directive: "provider"}}, Var: "call1", Expr: "mk()"})
	g.Node(&Assign{Base: Base{Phase: PhaseWire, Origin: Origin{Directive: "provider"}}, Var: "call_1", Expr: "mk2()"})
	g.Node(&Call{Base: Base{Phase: PhaseWire, Origin: Origin{Directive: "provider"}}, Fn: "registerThing"})
	mmd := g.Graph().Mermaid()
	if strings.Contains(mmd, "registerThing") {
		t.Fatalf("var-less nodes must not render:\n%s", mmd)
	}
	re := regexp.MustCompile(`(?m)^\s+(n\d+)[\[\({]`)
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(mmd, -1) {
		if seen[m[1]] {
			t.Fatalf("duplicate mermaid id %q:\n%s", m[1], mmd)
		}
		seen[m[1]] = true
	}
	if len(seen) != 2 {
		t.Fatalf("expected 2 distinct mermaid node ids, got %d:\n%s", len(seen), mmd)
	}
}

func TestGraphDefaultFlowIDIsReserved(t *testing.T) {
	g := New()
	g.SetModule("demo")
	g.SetDirective("cli:command")
	sc := g.AddScope("buildDefault", token.Position{})
	sc.nodes = append(sc.nodes, &Assign{Base: Base{Phase: PhaseWire, Origin: Origin{Directive: "provider"}}, Var: "svc", Expr: "mk()"})
	gr := g.Graph()
	var ids []string
	for _, f := range gr.Flows {
		ids = append(ids, f.ID)
	}
	if !reflect.DeepEqual(ids, []string{"default", "buildDefault"}) {
		t.Fatalf("a command named Default must not collide with the unscoped flow, got %v", ids)
	}
	dot := gr.DOT()
	if !strings.Contains(dot, "subgraph cluster_f1 {") || strings.Contains(dot, "subgraph cluster_f0 {") {
		t.Fatalf("DOT must render the command cluster and skip the empty default flow:\n%s", dot)
	}
	mmd := gr.Mermaid()
	if !strings.Contains(mmd, `subgraph f1["buildDefault"]`) {
		t.Fatalf("mermaid subgraphs must use sequential ids with titles:\n%s", mmd)
	}
}

func TestNodelessScopeFlowNamesNoFunction(t *testing.T) {
	pkg := typecheckScopePkg(t, "example.com/inline", "package inline\n\ntype V struct{}\n")
	vT := types.NewPointer(pkg.Scope().Lookup("V").Type())
	g := New()
	g.SetModule("demo")
	g.SetDirective("provider")
	g.BindLazy(vT, "", func() (string, diag.Diagnostics) { return "inline.New()", nil })
	g.AddScope("buildOnly", token.Position{}, ScopeRoot{Type: vT})
	if ds := g.MaterializeScopes(); ds.HasFatal() {
		t.Fatalf("MaterializeScopes: %v", ds)
	}
	if _, err := g.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	gr := g.Graph()
	flow := flowByID(t, gr, "only")
	if flow.Fn != "" {
		t.Fatalf("a nodeless scope emits no build function; flow.Fn = %q", flow.Fn)
	}
	if !strings.Contains(gr.DOT(), `label="only"`) {
		t.Fatalf("diagram title must fall back to the flow id:\n%s", gr.DOT())
	}
}

func TestRenderingsOmitBranchLocalEffects(t *testing.T) {
	g := New()
	g.SetModule("demo")
	g.SetDirective("provider:select")
	g.Node(&Assign{Base: Base{Phase: PhaseWire, Origin: Origin{Directive: "http"}}, Var: "r", Expr: "router.New()"})
	g.Node(&Select{
		Base: Base{Phase: PhaseWire, Origin: Origin{Directive: "provider:select"}},
		Var:  "svc", Iface: "demo.Svc", KeyExpr: `"x"`, FmtPkg: "fmt",
		Cases: []Case{{
			Value: "x",
			Body: []Node{&Route{
				Base:   Base{Origin: Origin{Directive: "http:handle"}},
				Router: "r", Kind: RouteHandleFunc, Pattern: "/branch", Handler: "demo.H",
			}},
			Result: Call{
				Base: Base{Origin: Origin{Directive: "provider"}},
				Fn:   "demo.NewSvc", Args: []string{"r"},
			},
		}},
	})
	gr := g.Graph()
	dot, mmd := gr.DOT(), gr.Mermaid()
	if strings.Contains(dot, "/branch") || strings.Contains(mmd, "/branch") {
		t.Fatalf("branch-local registration effects must not render:\n%s", dot)
	}
	if !strings.Contains(dot, "demo.NewSvc") || !strings.Contains(mmd, "demo.NewSvc") {
		t.Fatalf("select constructor must render in both diagrams:\n%s\n%s", dot, mmd)
	}
	if !strings.Contains(dot, `"default/call@0" -> "default/r";`) {
		t.Fatalf("select constructor edge missing from DOT:\n%s", dot)
	}
	if !strings.Contains(mmd, " --> ") {
		t.Fatalf("select constructor edge missing from mermaid:\n%s", mmd)
	}
}

func TestRenderingsSurviveJSONRoundTrip(t *testing.T) {
	gr := graphWorld(t).Graph()
	data, err := json.Marshal(gr)
	if err != nil {
		t.Fatal(err)
	}
	var back Graph
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.DOT() != gr.DOT() || back.Mermaid() != gr.Mermaid() {
		t.Fatal("renderings must be a function of the versioned artifact")
	}
	if !strings.Contains(back.DOT(), "app.NewPlainGreeter") {
		t.Fatal("non-error select implementation lost in round trip")
	}
}

func TestGraphNamedJSONSpelling(t *testing.T) {
	pkg := typecheckScopePkg(t, "example.com/spell", "package spell\n\ntype DB struct{}\n")
	db := types.NewPointer(pkg.Scope().Lookup("DB").Type())

	g := New()
	g.SetModule("example.com/spell")
	g.SetDirective("provider")
	g.Bind(db, "replica", "spell.NewReplica()")
	g.Node(&Call{Base: Base{Phase: PhaseWire}, Var: "db", Fn: "spell.NewReplica", BindingName: "replica"})
	g.AddScope("buildX", token.Position{}, ScopeRoot{Type: db, Name: "replica"})
	if _, err := g.Render(); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(g.Graph())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"name":"replica"`, `"bindingName":"replica"`} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("marshaled graph misses %s:\n%s", want, out)
		}
	}
	if name, ok := rootField(t, out, "name"); !ok || name != "replica" {
		t.Fatalf("root name field = %q, %v", name, ok)
	}

	unnamed := New()
	unnamed.SetModule("example.com/spell")
	unnamed.SetDirective("provider")
	unnamed.Bind(db, "", "spell.NewDB()")
	unnamed.Node(&Call{Base: Base{Phase: PhaseWire}, Var: "db", Fn: "spell.NewDB"})
	unnamed.AddScope("buildX", token.Position{}, ScopeRoot{Type: db})
	if _, err := unnamed.Render(); err != nil {
		t.Fatal(err)
	}
	plain, err := json.Marshal(unnamed.Graph())
	if err != nil {
		t.Fatal(err)
	}
	for _, stray := range []string{`"name"`, `"bindingName"`} {
		if strings.Contains(string(plain), stray) {
			t.Fatalf("unnamed graph must omit %s:\n%s", stray, plain)
		}
	}
}

// fragmentGraphWorld provides shared and flow-specific nodes for graph export tests.
func fragmentGraphWorld(t *testing.T) *Gen {
	t.Helper()
	pkg := typecheckScopePkg(t, "example.com/app", `package app

type Store struct{}

type Cache struct{}

type Greeter interface{ Greet() string }
`)
	store := types.NewPointer(pkg.Scope().Lookup("Store").Type())
	cache := types.NewPointer(pkg.Scope().Lookup("Cache").Type())
	greeter := pkg.Scope().Lookup("Greeter").Type()

	g := New()
	g.SetModule("example.com/app")
	g.FragmentMode()
	g.SetDirective("provider")
	g.BindLazy(store, "db", func() (string, diag.Diagnostics) {
		v := g.Var("conn")
		g.Node(&Call{
			Base: Base{Phase: PhaseWire, Origin: Origin{Directive: "provider"}},
			Var:  v, Fn: g.Import("example.com/app") + ".NewStore",
			Err: ErrReturn, Type: store,
		})
		return v, nil
	})
	g.BindLazy(greeter, "", func() (string, diag.Diagnostics) {
		conn, ds, ok := g.Instance(store, "db")
		if !ok {
			return "", ds
		}
		v := g.Var("greeter")
		g.Node(&Select{
			Base:    Base{Phase: PhaseWire, Origin: Origin{Directive: "provider:select"}},
			Var:     v,
			Iface:   "app.Greeter",
			KeyExpr: `"fancy"`,
			FmtPkg:  g.Import("fmt"),
			Cases: []Case{{
				Value: "fancy",
				Body: []Node{&Assign{
					Base: Base{Phase: PhaseWire, Origin: Origin{Directive: "provider"}},
					Var:  "tuned", Expr: "app.Tune(" + conn + ")",
				}},
				Result: Call{
					Base: Base{Origin: Origin{Directive: "provider"}},
					Var:  "greeterFancy", Fn: "app.NewFancyGreeter",
					Args: []string{"tuned"}, Type: greeter, Err: ErrNone,
				},
			}},
		})
		return v, ds
	})
	g.BindLazy(cache, "", func() (string, diag.Diagnostics) {
		conn, ds, ok := g.Instance(store, "db")
		if !ok {
			return "", ds
		}
		v := g.Var("cache")
		g.Node(&Call{
			Base: Base{Phase: PhaseWire, Origin: Origin{Directive: "provider"}},
			Var:  v, Fn: g.Import("example.com/app") + ".NewCache",
			Args: []string{conn}, Err: ErrReturn, Type: cache,
		})
		return v, ds
	})
	for _, cmd := range []struct {
		name  string
		roots []ScopeRoot
	}{
		{"alpha", []ScopeRoot{{Type: greeter}, {Type: cache}}},
		{"beta", []ScopeRoot{{Type: greeter}}},
	} {
		s := g.AddScope("build"+upperFirst(cmd.name), token.Position{}, cmd.roots...)
		g.AddCommandFunc(CommandFunc{
			Name:  cmd.name,
			Fn:    g.Import("example.com/app") + ".Run" + upperFirst(cmd.name),
			Scope: s,
		})
	}
	if ds := g.WalkFlows(); ds.HasFatal() {
		t.Fatalf("WalkFlows: %v", ds)
	}
	if ds := g.PlanFragments(); ds.HasFatal() {
		t.Fatalf("PlanFragments: %v", ds)
	}
	if _, err := g.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	return g
}

func TestGraphOccurrencesOfOneNodeShareALogicalID(t *testing.T) {
	gr := fragmentGraphWorld(t).Graph()

	alpha := nodeByID(t, gr, "alpha/conn")
	beta := nodeByID(t, gr, "beta/conn")
	if alpha.Node == "" || alpha.Node != beta.Node {
		t.Fatalf("per-flow occurrences must share a node id, got %q and %q", alpha.Node, beta.Node)
	}
	if alpha.ID == beta.ID {
		t.Fatalf("per-flow occurrences stay distinct entries, both %q", alpha.ID)
	}
	want := []string{"alpha", "beta"}
	if !reflect.DeepEqual(alpha.Usage, want) || !reflect.DeepEqual(beta.Usage, want) {
		t.Fatalf("usage = %v and %v, want %v", alpha.Usage, beta.Usage, want)
	}
}

func TestGraphFlattenedSelectChildrenInheritTheirOwner(t *testing.T) {
	gr := fragmentGraphWorld(t).Graph()

	sel := nodeByID(t, gr, "alpha/greeter")
	if sel.Node == "" {
		t.Fatalf("select node missing a logical id: %+v", sel)
	}
	body := nodeByID(t, gr, "alpha/tuned")
	if want := sel.Node + "/case@0/body@0"; body.Node != want {
		t.Fatalf("case body id = %q, want %q", body.Node, want)
	}
	result := nodeByID(t, gr, "alpha/greeterFancy")
	if want := sel.Node + "/case@0/result"; result.Node != want {
		t.Fatalf("case result id = %q, want %q", result.Node, want)
	}
	for _, child := range []GraphNode{body, result} {
		if !reflect.DeepEqual(child.Usage, sel.Usage) || child.Fragment != sel.Fragment {
			t.Fatalf("child %q = usage %v, fragment %q; want the owner's %v, %q",
				child.ID, child.Usage, child.Fragment, sel.Usage, sel.Fragment)
		}
	}
	if beta := nodeByID(t, gr, "beta/tuned"); beta.Node != body.Node {
		t.Fatalf("flattened children join across flows: %q != %q", beta.Node, body.Node)
	}
}

func TestGraphNodesNameTheirExtractedRegion(t *testing.T) {
	gr := fragmentGraphWorld(t).Graph()

	conn := nodeByID(t, gr, "alpha/conn")
	if conn.Fragment != "buildDb" {
		t.Fatalf("region member fragment = %q, want buildDb", conn.Fragment)
	}
	if sel := nodeByID(t, gr, "alpha/greeter"); sel.Fragment != "buildDb" {
		t.Fatalf("region member fragment = %q, want buildDb", sel.Fragment)
	}
	inline := nodeByID(t, gr, "alpha/cache")
	if inline.Fragment != "" {
		t.Fatalf("inline node must carry no fragment, got %q", inline.Fragment)
	}
	if !reflect.DeepEqual(inline.Usage, []string{"alpha"}) {
		t.Fatalf("inline usage = %v, want [alpha]", inline.Usage)
	}
}

func TestGraphListsExtractedFragments(t *testing.T) {
	gr := fragmentGraphWorld(t).Graph()

	conn := nodeByID(t, gr, "alpha/conn")
	want := []GraphFragment{{ID: conn.Node, Usage: []string{"alpha", "beta"}, Fn: "buildDb"}}
	if !reflect.DeepEqual(gr.Fragments, want) {
		t.Fatalf("fragments = %+v, want %+v", gr.Fragments, want)
	}
}

func TestGraphWithoutFragmentModeOmitsTheNewFields(t *testing.T) {
	out, err := json.Marshal(graphWorld(t).Graph())
	if err != nil {
		t.Fatal(err)
	}
	for _, stray := range []string{`"node":`, `"usage":`, `"fragment":`, `"fragments":`} {
		if strings.Contains(string(out), stray) {
			t.Fatalf("legacy graph must omit %s:\n%s", stray, out)
		}
	}
}

func rootField(t *testing.T, out []byte, field string) (string, bool) {
	t.Helper()
	var doc struct {
		Flows []struct {
			Roots []map[string]any `json:"roots"`
		} `json:"flows"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	for _, f := range doc.Flows {
		for _, r := range f.Roots {
			v, ok := r[field].(string)
			return v, ok
		}
	}
	t.Fatal("no scope root in marshaled graph")
	return "", false
}

func TestGraphJSONSpellsTheNewFields(t *testing.T) {
	g := fragmentGraphWorld(t)
	if _, err := g.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	data, err := json.Marshal(g.Graph())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	text := string(data)
	for _, key := range []string{`"node":`, `"usage":`, `"fragment":"buildDb"`, `"fragments":[`, `"fn":"buildDb"`} {
		if !strings.Contains(text, key) {
			t.Fatalf("marshaled graph missing %s:\n%s", key, text)
		}
	}

	legacy := New()
	legacy.SetModule("example.com/app")
	legacy.SetDirective("provider")
	legacy.Node(&Assign{Base: Base{Phase: PhaseWire}, Var: "v", Expr: "app.New()"})
	if _, err := legacy.Render(); err != nil {
		t.Fatalf("Render legacy: %v", err)
	}
	data, err = json.Marshal(legacy.Graph())
	if err != nil {
		t.Fatalf("marshal legacy: %v", err)
	}
	for _, key := range []string{`"node":`, `"usage":`, `"fragment":`, `"fragments":`} {
		if strings.Contains(string(data), key) {
			t.Fatalf("legacy graph must omit %s:\n%s", key, data)
		}
	}
}

func TestGraphFragmentsMatchRenderedFunctions(t *testing.T) {
	g := fragmentGraphWorld(t)
	src, err := g.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	gr := g.Graph()
	if len(gr.Fragments) == 0 {
		t.Fatal("no fragments exported")
	}
	exported := map[string]bool{}
	for _, f := range gr.Fragments {
		exported[f.Fn] = true
	}
	rendered := map[string]bool{}
	for _, line := range strings.Split(string(src), "\n") {
		if !strings.HasPrefix(line, "func ") {
			continue
		}
		name := strings.TrimPrefix(line, "func ")
		if i := strings.IndexByte(name, '('); i > 0 {
			name = name[:i]
		}
		if name != "run" && name != "main" {
			rendered[name] = true
		}
	}
	if !reflect.DeepEqual(exported, rendered) {
		t.Fatalf("exported fragments %v differ from rendered functions %v", exported, rendered)
	}
}

func TestGraphFragmentIDsDistinctAcrossSharedConfig(t *testing.T) {
	// A config load in disjoint regions keeps a distinct fragment ID in each flow.
	pkg := typecheckScopePkg(t, "example.com/app", `package app

type Cfg struct{}

type A struct{}

type B struct{}
`)
	cfg := types.NewPointer(pkg.Scope().Lookup("Cfg").Type())
	ta := types.NewPointer(pkg.Scope().Lookup("A").Type())
	tb := types.NewPointer(pkg.Scope().Lookup("B").Type())
	g := New()
	g.SetModule("example.com/app")
	g.FragmentMode()
	g.SetDirective("provider")
	g.BindLazy(cfg, "", func() (string, diag.Diagnostics) {
		g.Node(&ConfigLoad{
			Base: Base{Phase: PhaseConfig, Origin: Origin{Directive: "config"}},
			Var:  "appCfg", Pkg: g.Import("github.com/gofabrik/fabrik/config"),
			Type: "app.Cfg",
		})
		return "appCfg", nil
	})
	chain := func(tt types.Type, name, base string) {
		g.BindLazy(tt, name, func() (string, diag.Diagnostics) {
			cv, ds, ok := g.Instance(cfg, "")
			if !ok {
				return "", ds
			}
			v := g.Var(base)
			g.Node(&Call{Base: Base{Phase: PhaseWire, Origin: Origin{Directive: "provider"}},
				Var: v, Fn: g.Import("example.com/app") + ".New" + strings.ToUpper(base[:1]) + base[1:],
				Args: []string{cv}, Err: ErrReturn, Type: tt})
			v2 := g.Var(base + "Twin")
			g.Node(&Call{Base: Base{Phase: PhaseWire, Origin: Origin{Directive: "provider"}},
				Var: v2, Fn: g.Import("example.com/app") + ".Wrap", Args: []string{v}, Err: ErrNone, Type: tt})
			return v, ds
		})
	}
	chain(ta, "left", "alpha")
	chain(tb, "right", "bravo")
	addCmd := func(name string, root ScopeRoot) {
		s := g.AddScope("build"+upperFirst(name), token.Position{}, root)
		g.AddCommandFunc(CommandFunc{Name: name, Fn: "app.Run" + upperFirst(name), Scope: s})
	}
	addCmd("one", ScopeRoot{Type: ta, Name: "left"})
	addCmd("two", ScopeRoot{Type: ta, Name: "left"})
	addCmd("three", ScopeRoot{Type: tb, Name: "right"})
	addCmd("four", ScopeRoot{Type: tb, Name: "right"})
	if ds := g.WalkFlows(); ds.HasFatal() {
		t.Fatalf("WalkFlows: %v", ds)
	}
	if ds := g.PlanFragments(); ds.HasFatal() {
		t.Fatalf("PlanFragments: %v", ds)
	}
	if _, err := g.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	gr := g.Graph()
	if len(gr.Fragments) != 2 {
		t.Fatalf("fragments = %+v, want two regions", gr.Fragments)
	}
	if gr.Fragments[0].ID == gr.Fragments[1].ID {
		t.Fatalf("fragment ids must be distinct: %+v", gr.Fragments)
	}
}

func TestGraphReappendedNodeFoldsIntoOwningEntry(t *testing.T) {
	// Repeated occurrences share identity and usage without changing the region ID.
	pkg := typecheckScopePkg(t, "example.com/app", `package app

type Store struct{}
`)
	store := types.NewPointer(pkg.Scope().Lookup("Store").Type())
	g := New()
	g.SetModule("example.com/app")
	g.FragmentMode()
	g.SetDirective("provider")
	g.BindLazy(store, "db", func() (string, diag.Diagnostics) {
		v := g.Var("conn")
		g.Node(&Call{Base: Base{Phase: PhaseWire, Origin: Origin{Directive: "provider"}},
			Var: v, Fn: g.Import("example.com/app") + ".NewStore", Err: ErrReturn, Type: store})
		ping := &Call{Base: Base{Phase: PhaseWire, Origin: Origin{Directive: "provider"}},
			Fn: "app.Ping", Args: []string{v}, Err: ErrNone}
		g.Node(ping)
		g.Node(ping)
		return v, nil
	})
	for _, name := range []string{"alpha", "beta"} {
		s := g.AddScope("build"+upperFirst(name), token.Position{}, ScopeRoot{Type: store, Name: "db"})
		g.AddCommandFunc(CommandFunc{Name: name, Fn: "app.Run" + upperFirst(name), Scope: s})
	}
	if ds := g.WalkFlows(); ds.HasFatal() {
		t.Fatalf("WalkFlows: %v", ds)
	}
	if ds := g.PlanFragments(); ds.HasFatal() {
		t.Fatalf("PlanFragments: %v", ds)
	}
	if _, err := g.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	gr := g.Graph()
	if len(gr.Fragments) != 1 {
		t.Fatalf("fragments = %+v, want one region", gr.Fragments)
	}
	frag := gr.Fragments[0]
	if frag.ID == "" {
		t.Fatalf("fragment id empty: %+v", frag)
	}
	idOwned := false
	for _, n := range gr.Nodes {
		if n.Node == frag.ID && n.Fragment == frag.Fn {
			idOwned = true
		}
	}
	if !idOwned {
		t.Fatalf("fragment id %q names no node of region %q: %+v", frag.ID, frag.Fn, gr.Nodes)
	}
	var pings []GraphNode
	for _, n := range gr.Nodes {
		if n.Fn == "app.Ping" {
			pings = append(pings, n)
		}
	}
	if len(pings) < 2 {
		t.Fatalf("re-appended call missing occurrences: %+v", pings)
	}
	want := pings[0]
	for _, p := range pings[1:] {
		if p.Node != want.Node || !reflect.DeepEqual(p.Usage, want.Usage) || p.Fragment != want.Fragment {
			t.Fatalf("occurrences diverge: %+v vs %+v", want, p)
		}
	}
	if want.Fragment == "" || !reflect.DeepEqual(want.Usage, []string{"alpha", "beta"}) {
		t.Fatalf("folded attribution wrong: %+v", want)
	}
}
