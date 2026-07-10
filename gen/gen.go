package gen

import (
	"bytes"
	"fmt"
	"go/format"
	"go/types"
	"slices"
	"sort"
	"strings"

	"github.com/gofabrik/fabrik/diag"
	"golang.org/x/tools/go/types/typeutil"
)

// Phase orders generated statements inside run().
type Phase int

const (
	PhaseConfig     Phase = iota // configuration loading, before all else
	PhaseInit                    // setup calls that run before wiring
	PhaseWire                    // construct app and runtime values
	PhaseMiddleware              // global middleware registration
	PhaseRegister                // register generated behavior
	PhaseServe                   // start serving; must end with a return
)

var phaseLabels = []struct {
	phase Phase
	label string
}{
	{PhaseConfig, "Config"},
	{PhaseInit, "Init"},
	{PhaseWire, "Providers"},
	{PhaseMiddleware, "Middleware"},
	{PhaseRegister, "Routes"},
	{PhaseServe, "Serve"},
}

// lazyBind materializes a binding on first resolution.
type lazyBind struct {
	build   func() (string, diag.Diagnostics)
	owner   string
	running bool
}

// Gen assembles main.gen.go from imports, DI bindings, and phased statements.
type Gen struct {
	imports    map[string]string // import path -> alias
	idents     map[string]bool   // taken identifiers: aliases and vars
	binds      typeutil.Map      // types.Type -> map[string]string (name -> expr)
	lazy       typeutil.Map      // types.Type -> map[string]*lazyBind
	lazyByPath map[string]*lazyBind
	pathExprs  map[string]string // materialized path bindings, for InstancePath
	singletons map[string]string // singleton key -> var name
	nodes      []Node
	current    string // active directive
	module     string // module path of the generated app
	hints      []func(types.Type) (string, bool)

	materializing []string // active lazy-bind stack
}

// New returns an empty Gen.
func New() *Gen {
	return &Gen{
		imports:    map[string]string{},
		idents:     map[string]bool{},
		lazyByPath: map[string]*lazyBind{},
		pathExprs:  map[string]string{},
		singletons: map[string]string{},
	}
}

// SetModule records the module path of the app being generated.
func (g *Gen) SetModule(path string) { g.module = path }

// Module returns the module path of the app being generated.
func (g *Gen) Module() string { return g.module }

// SetDirective records the directive currently emitting code.
func (g *Gen) SetDirective(name string) { g.current = name }

// Import records an import and returns its stable alias.
func (g *Gen) Import(path string) string {
	base := path
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	return g.importAs(path, base)
}

// ImportPkg imports a typed package by its declared name.
func (g *Gen) ImportPkg(p *types.Package) string {
	return g.importAs(p.Path(), p.Name())
}

func (g *Gen) importAs(path, base string) string {
	if a, ok := g.imports[path]; ok {
		return a
	}
	alias := g.takeIdent(base)
	g.imports[path] = alias
	return alias
}

// Var reserves a unique identifier derived from base.
func (g *Gen) Var(base string) string { return g.takeIdent(base) }

func (g *Gen) takeIdent(base string) string {
	name := base
	for n := 2; g.idents[name]; n++ {
		name = fmt.Sprintf("%s%d", base, n)
	}
	g.idents[name] = true
	return name
}

// TypeExpr renders t and imports every package it mentions.
func (g *Gen) TypeExpr(t types.Type) string {
	return types.TypeString(t, func(p *types.Package) string { return g.ImportPkg(p) })
}

// Bind records expr as the wired value for (t, name).
func (g *Gen) Bind(t types.Type, name, expr string) {
	t = types.Unalias(t)
	m, _ := g.binds.At(t).(map[string]string)
	if m == nil {
		m = map[string]string{}
		g.binds.Set(t, m)
	}
	if _, dup := m[name]; dup {
		panic(fmt.Sprintf("gen: duplicate bind for %s %q", t, name))
	}
	m[name] = expr
}

// BindLazy registers a value that is emitted on first resolution.
func (g *Gen) BindLazy(t types.Type, name string, build func() (string, diag.Diagnostics)) {
	t = types.Unalias(t)
	m, _ := g.lazy.At(t).(map[string]*lazyBind)
	if m == nil {
		m = map[string]*lazyBind{}
		g.lazy.Set(t, m)
	}
	if _, dup := m[name]; dup {
		panic(fmt.Sprintf("gen: duplicate lazy bind for %s %q", t, name))
	}
	m[name] = &lazyBind{build: build, owner: g.current}
}

// BindLazyPath registers a lazy binding matched by a printed type path.
func (g *Gen) BindLazyPath(path string, build func() (string, diag.Diagnostics)) {
	if _, dup := g.lazyByPath[path]; dup {
		panic(fmt.Sprintf("gen: duplicate lazy bind for path %s", path))
	}
	g.lazyByPath[path] = &lazyBind{build: build, owner: g.current}
}

// Instance resolves the wired expression for (t, name).
func (g *Gen) Instance(t types.Type, name string) (string, diag.Diagnostics, bool) {
	t = types.Unalias(t)
	if m, _ := g.binds.At(t).(map[string]string); m != nil {
		if expr, ok := m[name]; ok {
			return expr, nil, true
		}
	}
	if path := types.TypeString(t, nil); name == "" {
		if expr, ok := g.pathExprs[path]; ok {
			// Bind the concrete type on first type-keyed lookup.
			g.Bind(t, name, expr)
			return expr, nil, true
		}
		if lb, ok := g.lazyByPath[path]; ok {
			expr, ds, ok := g.materialize(t, name, lb)
			if ok && len(ds) == 0 {
				g.pathExprs[path] = expr
			}
			return expr, ds, ok
		}
	}
	if m, _ := g.lazy.At(t).(map[string]*lazyBind); m != nil {
		if lb, ok := m[name]; ok {
			return g.materialize(t, name, lb)
		}
	}
	return "", nil, false
}

// HasBindingPath reports whether a path-keyed binding exists, lazy or
// already materialized.
func (g *Gen) HasBindingPath(path string) bool {
	if _, ok := g.pathExprs[path]; ok {
		return true
	}
	_, ok := g.lazyByPath[path]
	return ok
}

// InstancePath resolves a path-keyed lazy binding.
//
// Diagnosed path builds are not cached.
func (g *Gen) InstancePath(path string) (string, diag.Diagnostics, bool) {
	if expr, ok := g.pathExprs[path]; ok {
		return expr, nil, true
	}
	lb, ok := g.lazyByPath[path]
	if !ok {
		return "", nil, false
	}
	if lb.running {
		return "", diag.Diagnostics{g.cycleDiag(path)}, false
	}
	lb.running = true
	g.materializing = append(g.materializing, path)
	prev := g.current
	g.current = lb.owner
	// Engine recovery expects generation state to be restored after panics.
	defer func() {
		g.current = prev
		g.materializing = g.materializing[:len(g.materializing)-1]
		lb.running = false
	}()
	expr, ds := lb.build()
	if len(ds) == 0 {
		g.pathExprs[path] = expr
	}
	return expr, ds, true
}

// materialize runs a type-keyed lazy binding once.
func (g *Gen) materialize(t types.Type, name string, lb *lazyBind) (string, diag.Diagnostics, bool) {
	// Avoid adding imports while formatting cycle diagnostics.
	key := types.TypeString(t, func(p *types.Package) string { return p.Name() })
	if lb.running {
		return "", diag.Diagnostics{g.cycleDiag(key)}, false
	}
	lb.running = true
	g.materializing = append(g.materializing, key)
	prev := g.current
	g.current = lb.owner
	// Engine recovery expects generation state to be restored after panics.
	defer func() {
		g.current = prev
		g.materializing = g.materializing[:len(g.materializing)-1]
		lb.running = false
	}()
	expr, ds := lb.build()
	// Cache diagnosed type builds to avoid repeating shared dependency errors.
	g.Bind(t, name, expr)
	return expr, ds, true
}

// cycleDiag reports the active provider cycle ending at key.
func (g *Gen) cycleDiag(key string) diag.Diagnostic {
	chain := g.materializing
	for i, k := range g.materializing {
		if k == key {
			chain = g.materializing[i:]
			break
		}
	}
	return diag.Diagnostic{
		Severity: diag.SevError,
		Message:  "provider cycle: " + strings.Join(append(slices.Clone(chain), key), " -> "),
		Help:     "break the cycle by removing one of the dependencies",
	}
}

// Singleton returns the shared variable for key, emitting it on first use.
func (g *Gen) Singleton(key, varName, ctor string) string {
	return g.SingletonIn(PhaseWire, key, varName, ctor)
}

// SingletonIn emits the shared variable in a specific phase.
func (g *Gen) SingletonIn(phase Phase, key, varName, ctor string) string {
	if v, ok := g.singletons[key]; ok {
		return v
	}
	v := g.takeIdent(varName)
	g.singletons[key] = v
	g.Node(&Assign{Base: Base{Phase: phase}, Var: v, Expr: ctor})
	return v
}

// HasBinding reports whether (t, name) has an eager or lazy binding,
// without materializing anything.
func (g *Gen) HasBinding(t types.Type, name string) bool {
	t = types.Unalias(t)
	if m, _ := g.binds.At(t).(map[string]string); m != nil {
		if _, ok := m[name]; ok {
			return true
		}
	}
	if m, _ := g.lazy.At(t).(map[string]*lazyBind); m != nil {
		if _, ok := m[name]; ok {
			return true
		}
	}
	if _, ok := g.lazyByPath[types.TypeString(t, nil)]; ok && name == "" {
		return true
	}
	return false
}

// HasSingleton reports whether key has been created.
func (g *Gen) HasSingleton(key string) bool {
	_, ok := g.singletons[key]
	return ok
}

// Stmt appends a Raw node.
func (g *Gen) Stmt(phase Phase, format string, a ...any) {
	g.Node(&Raw{
		Base:  Base{Phase: phase},
		Lines: strings.Split(fmt.Sprintf(format, a...), "\n"),
	})
}

// Render assembles and formats main.gen.go.
func (g *Gen) Render() ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("// Code generated by fabrik. DO NOT EDIT.\n// Regenerate with: fabrik wire\n\npackage main\n\n")
	g.writeImports(&b)
	b.WriteString("func run() error {\n")
	served := false
	first := true
	for _, pl := range phaseLabels {
		var nodes []phaseNode
		for i, n := range g.nodes {
			if n.base().Phase == pl.phase {
				nodes = append(nodes, phaseNode{n: n, emit: i})
			}
		}
		if len(nodes) == 0 {
			continue
		}
		if !first {
			b.WriteString("\n")
		}
		first = false
		b.WriteString("// " + pl.label + "\n")

		clusters := layoutPhase(nodes)
		for ci, cluster := range clusters {
			if ci > 0 && (spacedCluster(cluster) || spacedCluster(clusters[ci-1])) {
				b.WriteString("\n")
			}
			for _, pn := range cluster {
				if l := pn.n.base().Label; l != "" {
					b.WriteString("// " + l + "\n")
				}
				for _, line := range renderNode(pn.n) {
					b.WriteString(line)
					b.WriteString("\n")
				}
			}
		}
		if pl.phase == PhaseServe {
			served = true
		}
	}
	if !served {
		if !first {
			b.WriteString("\n")
		}
		b.WriteString("return nil\n")
	}
	b.WriteString("}\n")

	src, err := format.Source(b.Bytes())
	if err != nil {
		if directive, text, found := g.findUnparsable(); found {
			return nil, fmt.Errorf("directive %q emitted an unparsable statement:\n%s", directive, text)
		}
		return nil, fmt.Errorf("format generated source: %w\n%s", err, b.String())
	}
	return src, nil
}

// writeImports renders standard, external, and app imports.
func (g *Gen) writeImports(b *bytes.Buffer) {
	if len(g.imports) == 0 {
		return
	}
	paths := make([]string, 0, len(g.imports))
	for p := range g.imports {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var groups [3][]string
	for _, p := range paths {
		line := g.importLine(p)
		switch {
		case g.module != "" && (p == g.module || strings.HasPrefix(p, g.module+"/")):
			groups[2] = append(groups[2], line)
		case strings.Contains(strings.SplitN(p, "/", 2)[0], "."):
			groups[1] = append(groups[1], line)
		default:
			groups[0] = append(groups[0], line)
		}
	}
	b.WriteString("import (\n")
	wrote := false
	for _, group := range groups {
		if len(group) == 0 {
			continue
		}
		if wrote {
			b.WriteString("\n")
		}
		wrote = true
		b.WriteString(strings.Join(group, "\n"))
		b.WriteString("\n")
	}
	b.WriteString(")\n\n")
}

func (g *Gen) importLine(path string) string {
	alias := g.imports[path]
	if i := strings.LastIndexByte(path, '/'); path[i+1:] == alias {
		return fmt.Sprintf("%q", path)
	}
	return fmt.Sprintf("%s %q", alias, path)
}

// findUnparsable locates the first node that fails to parse on its own.
func (g *Gen) findUnparsable() (directive, text string, found bool) {
	for _, n := range g.nodes {
		text := strings.Join(renderNode(n), "\n")
		probe := "package p\nfunc _() error {\n" + text + "\nreturn nil\n}\n"
		if _, err := format.Source([]byte(probe)); err != nil {
			return n.base().Origin.Directive, text, true
		}
	}
	return "", "", false
}
