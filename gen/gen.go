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

// Phase orders generated statements within run(): construction first, then hooks.
type Phase int

const (
	PhaseConfig     Phase = iota // configuration loading, before all else
	PhaseSetup                   // setup hooks: config exists, providers do not
	PhaseWire                    // construct app and runtime values
	PhaseMiddleware              // global middleware registration
	PhaseRegister                // register generated behavior onto constructed values
	PhasePrepare                 // prepare hooks: pre-intake work on registered resources
	PhaseStart                   // start hooks: start background runtime processes
)

var phaseLabels = []struct {
	phase Phase
	label string
}{
	{PhaseConfig, "Config"},
	{PhaseSetup, "Setup: after config, before providers"},
	{PhaseWire, "Providers"},
	{PhaseMiddleware, "Middleware"},
	{PhaseRegister, "Register"},
	{PhasePrepare, "Prepare: after registration, before runtime start"},
	{PhaseStart, "Start: after prepare, before serving"},
}

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
	ctxNeeded  bool   // context.Background assignment is needed
	ctxPkg     string // context import alias, registered at request time
	ctxVar     string // reserved context identifier, usually "ctx"
	current    string // active directive
	module     string // module path of the generated app
	hints      []func(types.Type) (string, bool)
	types      map[string]*types.Package // import path -> package, for LookupType

	materializing []string // active lazy-bind stack

	varTypeExpr map[string]string     // var identifier -> pre-rendered type (explicit)
	varTypeVal  map[string]types.Type // var identifier -> type, rendered lazily for entrypoint params
	entrypoints []Entrypoint          // runtime start functions the runner invokes
	commands    []string
}

// New returns an empty Gen.
func New() *Gen {
	return &Gen{
		imports:     map[string]string{},
		idents:      map[string]bool{},
		lazyByPath:  map[string]*lazyBind{},
		pathExprs:   map[string]string{},
		singletons:  map[string]string{},
		varTypeExpr: map[string]string{},
		varTypeVal:  map[string]types.Type{},
	}
}

// SetModule records the module path of the app being generated.
func (g *Gen) SetModule(path string) { g.module = path }

// SetTypes supplies type-checked packages for [Gen.LookupType].
func (g *Gen) SetTypes(m map[string]*types.Package) { g.types = m }

// LookupType returns a named type from a type-checked imported package.
func (g *Gen) LookupType(pkgPath, name string) (types.Type, bool) {
	pkg := g.types[pkgPath]
	if pkg == nil {
		return nil, false
	}
	obj := pkg.Scope().Lookup(name)
	if obj == nil {
		return nil, false
	}
	return obj.Type(), true
}

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
	g.recordVarType(expr, t)
}

// recordVarType defers type rendering to avoid imports used only by entrypoint parameters.
func (g *Gen) recordVarType(expr string, t types.Type) {
	if isIdent(expr) {
		if _, ok := g.varTypeVal[expr]; !ok {
			g.varTypeVal[expr] = t
		}
	}
}

// RecordVarType records the type of a variable built outside type-keyed bindings.
func (g *Gen) RecordVarType(name, typeExpr string) {
	if _, ok := g.varTypeExpr[name]; !ok {
		g.varTypeExpr[name] = typeExpr
	}
}

func (g *Gen) resolveVarType(name string) (string, bool) {
	if s, ok := g.varTypeExpr[name]; ok {
		return s, true
	}
	if t, ok := g.varTypeVal[name]; ok {
		return g.TypeExpr(t), true
	}
	return "", false
}

// Entrypoint describes a runtime function whose Uses become its typed parameters.
type Entrypoint struct {
	Name string
	Uses []string // run() vars the body needs, in parameter order
	Body []string
}

// AddEntrypoint registers a runtime start function and deduplicates its parameters.
func (g *Gen) AddEntrypoint(name string, uses, body []string) {
	seen := map[string]bool{}
	dedup := make([]string, 0, len(uses))
	for _, u := range uses {
		if !seen[u] {
			seen[u] = true
			dedup = append(dedup, u)
		}
	}
	g.entrypoints = append(g.entrypoints, Entrypoint{Name: name, Uses: dedup, Body: body})
}

// EntrypointCount reports how many runtime start functions are registered.
func (g *Gen) EntrypointCount() int { return len(g.entrypoints) }

// AddCommand registers a command factory expression evaluated in the generated run scope.
func (g *Gen) AddCommand(callExpr string) { g.commands = append(g.commands, callExpr) }

// CommandCount reports how many commands are registered.
func (g *Gen) CommandCount() int { return len(g.commands) }

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

// BindPath publishes an expression while its lazy path binding is materializing.
func (g *Gen) BindPath(path, expr string) { g.pathExprs[path] = expr }

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

// InstancePath resolves a path-keyed lazy binding without caching diagnosed builds.
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
	defer func() {
		g.current = prev
		g.materializing = g.materializing[:len(g.materializing)-1]
		lb.running = false
	}()
	expr, ds := lb.build()
	// BindPath may populate the type-keyed cache during the build.
	if !g.typeBound(t, name) {
		g.Bind(t, name, expr)
	}
	return expr, ds, true
}

func (g *Gen) typeBound(t types.Type, name string) bool {
	m, _ := g.binds.At(types.Unalias(t)).(map[string]string)
	if m == nil {
		return false
	}
	_, ok := m[name]
	return ok
}

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

// HasProviderBinding reports whether (t, name) has a provider-owned lazy binding, excluding library path bindings.
func (g *Gen) HasProviderBinding(t types.Type, name string) bool {
	t = types.Unalias(t)
	if m, _ := g.lazy.At(t).(map[string]*lazyBind); m != nil {
		_, ok := m[name]
		return ok
	}
	return false
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
	if len(g.entrypoints) > 0 || len(g.commands) > 0 {
		g.Context()
	}

	// Resolve entrypoint parameter types before writing imports because rendering types registers their packages.
	varType := map[string]string{}
	for _, e := range g.entrypoints {
		for _, u := range e.Uses {
			if _, ok := varType[u]; ok {
				continue
			}
			typ, ok := g.resolveVarType(u)
			if !ok {
				return nil, fmt.Errorf("gen: entrypoint var %q has no recorded type", u)
			}
			varType[u] = typ
		}
	}

	hasEntry := len(g.entrypoints) > 0
	hasCmd := len(g.commands) > 0
	needsCtx := hasEntry || hasCmd || usesCtx(g.nodes, g.ctxVar)

	// Imports must be registered before the import block is written.
	ctxPkg := ""
	if needsCtx {
		ctxPkg = g.Import("context")
	}
	if hasEntry || hasCmd {
		g.Import("os")
		g.Import("os/signal")
		g.Import("syscall")
		g.Import("errors")
	}
	if hasCmd {
		g.Import("fmt")
		g.Import("github.com/gofabrik/fabrik/cli")
	}

	var b bytes.Buffer
	b.WriteString("// Code generated by fabrik. DO NOT EDIT.\n// Regenerate with: fabrik wire\n\npackage main\n\n")
	g.writeImports(&b)

	g.writeRun(&b, needsCtx, ctxPkg)
	for _, e := range g.entrypoints {
		g.writeEntrypoint(&b, e, varType, ctxPkg)
	}

	src, err := format.Source(b.Bytes())
	if err != nil {
		if directive, text, found := g.findUnparsable(); found {
			return nil, fmt.Errorf("directive %q emitted an unparsable statement:\n%s", directive, text)
		}
		return nil, fmt.Errorf("format generated source: %w\n%s", err, b.String())
	}
	return src, nil
}

func (g *Gen) writeRun(b *bytes.Buffer, needsCtx bool, ctxPkg string) {
	if len(g.commands) > 0 {
		g.writeRunCommands(b, ctxPkg)
		return
	}
	ctx := g.ctxVar
	hasEntry := len(g.entrypoints) > 0
	if hasEntry {
		b.WriteString("func run() (err error) {\n")
		osp := g.Import("os")
		sigp := g.Import("os/signal")
		sysp := g.Import("syscall")
		errPkg := g.Import("errors")
		fmt.Fprintf(b, "%s, cancel := %s.WithCancel(%s.Background())\n", ctx, ctxPkg, ctxPkg)
		b.WriteString("defer cancel()\n")
		// Signal-driven cancellation during startup is a clean shutdown.
		fmt.Fprintf(b, "defer func() {\nif %s.Is(err, %s.Canceled) && %s.Err() != nil {\nerr = nil\n}\n}()\n", errPkg, ctxPkg, ctx)
		fmt.Fprintf(b, "sigc := make(chan %s.Signal, 2)\n", osp)
		fmt.Fprintf(b, "%s.Notify(sigc, %s.Interrupt, %s.SIGTERM)\n", sigp, osp, sysp)
		fmt.Fprintf(b, "defer %s.Stop(sigc)\n", sigp)
		fmt.Fprintf(b, "go func() {\n<-sigc\ncancel()\n<-sigc\n%s.Exit(1)\n}()\n\n", osp)
	} else {
		b.WriteString("func run() error {\n")
		if needsCtx {
			fmt.Fprintf(b, "%s := %s.Background()\n", ctx, ctxPkg)
		}
	}

	g.emitPhaseNodes(b, g.nodes, PhaseConfig, PhaseSetup, PhaseWire, PhaseMiddleware, PhaseRegister, PhasePrepare, PhaseStart)

	if hasEntry {
		g.writeEntrypointRunner(b, ctx, ctxPkg)
	} else {
		b.WriteString("return nil\n")
	}
	b.WriteString("}\n\n")
}

// writeRunCommands emits an integer-returning run that builds the graph before dispatch and defers runtime phases to the default handler.
func (g *Gen) writeRunCommands(b *bytes.Buffer, ctxPkg string) {
	ctx := g.ctxVar
	osp := g.Import("os")
	sigp := g.Import("os/signal")
	sysp := g.Import("syscall")
	errPkg := g.Import("errors")
	fmtp := g.Import("fmt")
	clip := g.Import("github.com/gofabrik/fabrik/cli")
	name := appName(g.module)

	b.WriteString("func run() int {\n")
	fmt.Fprintf(b, "%s, cancel := %s.WithCancel(%s.Background())\n", ctx, ctxPkg, ctxPkg)
	b.WriteString("defer cancel()\n")
	fmt.Fprintf(b, "sigc := make(chan %s.Signal, 2)\n", osp)
	fmt.Fprintf(b, "%s.Notify(sigc, %s.Interrupt, %s.SIGTERM)\n", sigp, osp, sysp)
	fmt.Fprintf(b, "defer %s.Stop(sigc)\n", sigp)
	fmt.Fprintf(b, "go func() {\n<-sigc\ncancel()\n<-sigc\n%s.Exit(1)\n}()\n\n", osp)

	fmt.Fprintf(b, "var root *%s.Command\n", clip)
	b.WriteString("err := func() error {\n")
	// Only graph construction runs before CLI dispatch.
	g.emitPhaseNodes(b, g.nodes, PhaseConfig, PhaseSetup, PhaseWire, PhaseMiddleware, PhaseRegister)
	b.WriteString("\n")
	fmt.Fprintf(b, "root = &%s.Command{\n", clip)
	fmt.Fprintf(b, "Name: %q,\n", name)
	hasEntry := len(g.entrypoints) > 0
	if hasEntry || g.hasNodesInPhases(PhasePrepare, PhaseStart) {
		fmt.Fprintf(b, "Run: func(cctx %s.Context) error {\n", clip)
		g.emitPhaseNodes(b, g.nodes, PhasePrepare, PhaseStart)
		if hasEntry {
			b.WriteString("\n")
			g.writeEntrypointFanout(b, "cctx", ctxPkg)
		} else {
			b.WriteString("return nil\n")
		}
		b.WriteString("},\n")
	}
	fmt.Fprintf(b, "Subcommands: []*%s.Command{\n", clip)
	for _, c := range g.commands {
		fmt.Fprintf(b, "%s,\n", c)
	}
	b.WriteString("},\n}\n")
	b.WriteString("return nil\n")
	b.WriteString("}()\n")
	b.WriteString("if err != nil {\n")
	fmt.Fprintf(b, "if %s.Is(err, %s.Canceled) && %s.Err() != nil {\nreturn 130\n}\n", errPkg, ctxPkg, ctx)
	fmt.Fprintf(b, "%s.Fprintln(%s.Stderr, %q, err)\n", fmtp, osp, name+":")
	b.WriteString("return 1\n}\n")
	fmt.Fprintf(b, "return root.Exec(%s.Args[1:], %s.WithSignalContext(%s))\n", osp, clip, ctx)
	b.WriteString("}\n\n")
}

// writeEntrypointFanout starts entrypoints on a handler-local context and cancels peers after the first non-cancellation error.
func (g *Gen) writeEntrypointFanout(b *bytes.Buffer, cctx, ctxPkg string) {
	errPkg := g.Import("errors")
	n := len(g.entrypoints)
	fmt.Fprintf(b, "ectx, ecancel := %s.WithCancel(%s)\n", ctxPkg, cctx)
	b.WriteString("defer ecancel()\n")
	fmt.Fprintf(b, "errc := make(chan error, %d)\n", n)
	for _, e := range g.entrypoints {
		args := append([]string{"ectx"}, e.Uses...)
		fmt.Fprintf(b, "go func() { errc <- %s(%s) }()\n", e.Name, strings.Join(args, ", "))
	}
	b.WriteString("var result error\n")
	fmt.Fprintf(b, "for range %d {\n", n)
	fmt.Fprintf(b, "if e := <-errc; e != nil && !%s.Is(e, %s.Canceled) && result == nil {\n", errPkg, ctxPkg)
	b.WriteString("result = e\n")
	b.WriteString("ecancel()\n")
	b.WriteString("}\n}\n")
	b.WriteString("return result\n")
}

func (g *Gen) hasNodesInPhases(phases ...Phase) bool {
	want := map[Phase]bool{}
	for _, p := range phases {
		want[p] = true
	}
	for _, n := range g.nodes {
		if want[n.base().Phase] {
			return true
		}
	}
	return false
}

func appName(module string) string {
	if module == "" {
		return "app"
	}
	if i := strings.LastIndexByte(module, '/'); i >= 0 {
		return module[i+1:]
	}
	return module
}

// writeEntrypointRunner cancels on the first non-cancellation error and waits for all entrypoints to
// stop; each bounds its own drain, so the wait needs no timeout.
func (g *Gen) writeEntrypointRunner(b *bytes.Buffer, ctx, ctxPkg string) {
	errPkg := g.Import("errors")
	n := len(g.entrypoints)
	fmt.Fprintf(b, "\nerrc := make(chan error, %d)\n", n)
	for _, e := range g.entrypoints {
		args := append([]string{ctx}, e.Uses...)
		fmt.Fprintf(b, "go func() { errc <- %s(%s) }()\n", e.Name, strings.Join(args, ", "))
	}
	b.WriteString("var result error\n")
	fmt.Fprintf(b, "for range %d {\n", n)
	fmt.Fprintf(b, "if e := <-errc; e != nil && !%s.Is(e, %s.Canceled) && result == nil {\n", errPkg, ctxPkg)
	b.WriteString("result = e\n")
	b.WriteString("cancel()\n")
	b.WriteString("}\n}\n")
	b.WriteString("return result\n")
}

func (g *Gen) writeEntrypoint(b *bytes.Buffer, e Entrypoint, varType map[string]string, ctxPkg string) {
	params := []string{g.ctxVar + " " + ctxPkg + ".Context"}
	for _, u := range e.Uses {
		params = append(params, u+" "+varType[u])
	}
	fmt.Fprintf(b, "func %s(%s) error {\n", e.Name, strings.Join(params, ", "))
	for _, line := range e.Body {
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("}\n\n")
}

func (g *Gen) emitPhaseNodes(b *bytes.Buffer, allNodes []Node, phases ...Phase) {
	want := map[Phase]bool{}
	for _, p := range phases {
		want[p] = true
	}
	first := true
	for _, pl := range phaseLabels {
		if !want[pl.phase] {
			continue
		}
		var nodes []phaseNode
		for i, n := range allNodes {
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
	}
}

func usesCtx(nodes []Node, ctxVar string) bool {
	if ctxVar == "" {
		return false
	}
	filter := map[string]bool{ctxVar: true}
	for _, n := range nodes {
		if len(uses(n, filter)) > 0 {
			return true
		}
	}
	return false
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
