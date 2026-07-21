package gen

import (
	"bytes"
	"fmt"
	"go/format"
	"go/token"
	"go/types"
	"slices"
	"sort"
	"strings"
	"unicode"

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

	commandFuncs  []CommandFunc
	commandGroups []CommandGroup
	commandRoot   *RootSpec
	// treeOrder preserves arrival order: commands use positive indices, groups negative, and root zero.
	treeOrder []int
	prologues []func() diag.Diagnostics

	scopes      []*Scope
	scope       *Scope          // active scope; nil uses default state
	aliasIdents map[string]bool // import aliases reserved by new scopes
}

// New returns an empty Gen.
func New() *Gen {
	return &Gen{
		imports:     map[string]string{},
		idents:      map[string]bool{},
		lazyByPath:  map[string]*lazyBind{},
		pathExprs:   map[string]string{},
		singletons:  map[string]string{},
		aliasIdents: map[string]bool{},
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
	if sc := g.scope; sc != nil && sc.validation {
		// Validation imports must not enter rendered output.
		if a, ok := sc.imports[path]; ok {
			return a
		}
		sc.imports[path] = base
		return base
	}
	alias := base
	for n := 2; g.idents[alias] || (g.scope != nil && g.scope.idents[alias]); n++ {
		alias = fmt.Sprintf("%s%d", base, n)
	}
	g.idents[alias] = true
	g.imports[path] = alias
	g.aliasIdents[alias] = true
	if sc := g.scope; sc != nil {
		sc.idents[alias] = true
	}
	return alias
}

// Var reserves a unique identifier derived from base within the active scope.
func (g *Gen) Var(base string) string {
	if sc := g.scope; sc != nil {
		name := base
		for n := 2; sc.idents[name]; n++ {
			name = fmt.Sprintf("%s%d", base, n)
		}
		sc.idents[name] = true
		return name
	}
	return g.takeIdent(base)
}

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
	if sc := g.scope; sc != nil {
		key := types.TypeString(t, nil)
		m := sc.binds[key]
		if m == nil {
			m = map[string]string{}
			sc.binds[key] = m
		}
		if _, dup := m[name]; dup {
			panic(fmt.Sprintf("gen: duplicate bind for %s %q", t, name))
		}
		m[name] = expr
		return
	}
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

// CommandFunc describes a generated CLI command and its dependency scope.
type CommandFunc struct {
	Name    string
	Help    string
	Long    string
	Usage   string
	Aliases []string
	Hidden  bool
	Fn      string // package-qualified user function

	// Path is the full command path; an empty path uses Name, and shared prefixes create intermediate nodes.
	Path []string

	// Inputs are emitted before the tree so wrappers and fields share handles.
	Inputs []CommandInput

	// ValueExprs follow scope roots in command parameter order.
	ValueExprs []string

	Use      []string // package-qualified cli.Middleware expressions, in order
	Examples []CommandExample
	Scope    *Scope
	Pos      token.Position
}

// CommandGroup decorates a non-executable tree node with metadata and inherited inputs.
type CommandGroup struct {
	Path     []string
	Help     string
	Long     string
	Usage    string
	Aliases  []string
	Hidden   bool
	Inputs   []CommandInput
	Use      []string // package-qualified cli.Middleware expressions, in order
	Examples []CommandExample
}

// RootSpec decorates the generated root command.
type RootSpec struct {
	Usage    string
	Version  string
	Long     string
	Inputs   []CommandInput
	Use      []string // package-qualified cli.Middleware expressions, in order
	Examples []CommandExample
}

// AddCommandGroup registers group metadata for one path.
func (g *Gen) AddCommandGroup(cg CommandGroup) {
	g.commandGroups = append(g.commandGroups, cg)
	g.treeOrder = append(g.treeOrder, -len(g.commandGroups))
}

// SetCommandRoot registers the root command's surface.
func (g *Gen) SetCommandRoot(r RootSpec) {
	g.commandRoot = &r
	g.treeOrder = append(g.treeOrder, 0)
}

// CommandInput is one flag or argument handle declaration.
type CommandInput struct {
	Var     string // local name, pre-allocated via Var
	Builder string // full builder expression for the handle
	Arg     bool   // positional argument rather than flag
}

// CommandExample is one --help Examples entry.
type CommandExample struct {
	Cmd  string
	Help string
}

// AddCommandFunc registers a command shell for the generated tree.
func (g *Gen) AddCommandFunc(c CommandFunc) {
	g.commandFuncs = append(g.commandFuncs, c)
	g.treeOrder = append(g.treeOrder, len(g.commandFuncs))
}

// CommandCount reports how many commands are registered.
func (g *Gen) CommandCount() int { return len(g.commandFuncs) }

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
func (g *Gen) BindPath(path, expr string) {
	if sc := g.scope; sc != nil {
		sc.pathExprs[path] = expr
		return
	}
	g.pathExprs[path] = expr
}

// BindLazyPath registers a lazy binding matched by a printed type path.
func (g *Gen) BindLazyPath(path string, build func() (string, diag.Diagnostics)) {
	if _, dup := g.lazyByPath[path]; dup {
		panic(fmt.Sprintf("gen: duplicate lazy bind for path %s", path))
	}
	g.lazyByPath[path] = &lazyBind{build: build, owner: g.current}
}

// Instance resolves (t, name) with scope-local state and shared lazy definitions.
func (g *Gen) Instance(t types.Type, name string) (string, diag.Diagnostics, bool) {
	t = types.Unalias(t)
	if sc := g.scope; sc != nil {
		key := types.TypeString(t, nil)
		if m := sc.binds[key]; m != nil {
			if expr, ok := m[name]; ok {
				return expr, nil, true
			}
		}
		if name == "" {
			if expr, ok := sc.pathExprs[key]; ok {
				g.Bind(t, name, expr)
				return expr, nil, true
			}
			if lb, ok := g.lazyByPath[key]; ok {
				expr, ds, ok := g.materialize(t, name, lb)
				if ok && len(ds) == 0 {
					sc.pathExprs[key] = expr
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
	if sc := g.scope; sc != nil {
		if expr, ok := sc.pathExprs[path]; ok {
			return expr, nil, true
		}
		return g.materializePath(path)
	}
	if expr, ok := g.pathExprs[path]; ok {
		return expr, nil, true
	}
	return g.materializePath(path)
}

func (g *Gen) materializePath(path string) (string, diag.Diagnostics, bool) {
	lb, ok := g.lazyByPath[path]
	if !ok {
		return "", nil, false
	}
	if g.lazyRunning(lb) {
		return "", diag.Diagnostics{g.cycleDiag(path)}, false
	}
	g.setLazyRunning(lb, true)
	g.materializing = append(g.materializing, path)
	prev := g.current
	g.current = lb.owner
	defer func() {
		g.current = prev
		g.materializing = g.materializing[:len(g.materializing)-1]
		g.setLazyRunning(lb, false)
	}()
	expr, ds := lb.build()
	if len(ds) == 0 {
		g.BindPath(path, expr)
	}
	return expr, ds, true
}

func (g *Gen) lazyRunning(lb *lazyBind) bool {
	if sc := g.scope; sc != nil {
		return sc.running[lb]
	}
	return lb.running
}

func (g *Gen) setLazyRunning(lb *lazyBind, v bool) {
	if sc := g.scope; sc != nil {
		sc.running[lb] = v
		return
	}
	lb.running = v
}

func (g *Gen) materialize(t types.Type, name string, lb *lazyBind) (string, diag.Diagnostics, bool) {
	// Avoid adding imports while formatting cycle diagnostics.
	key := types.TypeString(t, func(p *types.Package) string { return p.Name() })
	if g.lazyRunning(lb) {
		return "", diag.Diagnostics{g.cycleDiag(key)}, false
	}
	g.setLazyRunning(lb, true)
	g.materializing = append(g.materializing, key)
	prev := g.current
	g.current = lb.owner
	defer func() {
		g.current = prev
		g.materializing = g.materializing[:len(g.materializing)-1]
		g.setLazyRunning(lb, false)
	}()
	expr, ds := lb.build()
	// BindPath may populate the type-keyed cache during the build.
	if !g.typeBound(t, name) {
		g.Bind(t, name, expr)
	}
	return expr, ds, true
}

func (g *Gen) typeBound(t types.Type, name string) bool {
	t = types.Unalias(t)
	if sc := g.scope; sc != nil {
		m := sc.binds[types.TypeString(t, nil)]
		if m == nil {
			return false
		}
		_, ok := m[name]
		return ok
	}
	m, _ := g.binds.At(t).(map[string]string)
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

// SingletonIn emits one variable per key in the default flow or active scope.
func (g *Gen) SingletonIn(phase Phase, key, varName, ctor string) string {
	if sc := g.scope; sc != nil {
		if v, ok := sc.singletons[key]; ok {
			return v
		}
		v := g.Var(varName)
		sc.singletons[key] = v
		g.Node(&Assign{Base: Base{Phase: phase}, Var: v, Expr: ctor})
		return v
	}
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
	hasCmd := len(g.commandFuncs) > 0 || len(g.commandGroups) > 0 || g.commandRoot != nil
	if hasCmd {
		g.Context()
	}
	needsCtx := hasCmd || usesCtx(g.nodes, g.ctxVar)

	// Imports must be registered before the import block is written.
	ctxPkg := ""
	if needsCtx {
		ctxPkg = g.Import("context")
	}
	g.Import("os")
	if hasCmd {
		g.Import("os/signal")
		g.Import("syscall")
		g.Import("github.com/gofabrik/fabrik/cli")
	} else {
		g.Import("fmt")
	}

	if len(g.scopes) > 0 {
		g.Import("context")
	}

	var b bytes.Buffer
	b.WriteString("// Code generated by fabrik. DO NOT EDIT.\n// Regenerate with: fabrik wire\n\npackage main\n\n")
	g.writeImports(&b)

	g.writeRun(&b, needsCtx, ctxPkg)
	g.writeScopeFuncs(&b)

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
	if len(g.commandFuncs) > 0 || len(g.commandGroups) > 0 || g.commandRoot != nil {
		g.writeRunCommands(b, ctxPkg)
		return
	}
	osp := g.Import("os")
	fmtp := g.Import("fmt")
	b.WriteString("func run() int {\n")
	if needsCtx {
		fmt.Fprintf(b, "%s := %s.Background()\n", g.ctxVar, ctxPkg)
	}
	b.WriteString("if err := func() error {\n")
	g.emitPhaseNodes(b, g.nodes, PhaseConfig, PhaseSetup, PhaseWire, PhaseMiddleware, PhaseRegister)
	b.WriteString("return nil\n")
	b.WriteString("}(); err != nil {\n")
	fmt.Fprintf(b, "%s.Fprintln(%s.Stderr, %q, err)\n", fmtp, osp, appName(g.module)+":")
	b.WriteString("return 1\n}\n")
	b.WriteString("return 0\n")
	b.WriteString("}\n\n")
}

func (g *Gen) writeRunCommands(b *bytes.Buffer, ctxPkg string) {
	ctx := g.ctxVar
	osp := g.Import("os")
	sigp := g.Import("os/signal")
	sysp := g.Import("syscall")
	clip := g.Import("github.com/gofabrik/fabrik/cli")
	name := appName(g.module)

	b.WriteString("func run() int {\n")
	fmt.Fprintf(b, "%s, cancel := %s.WithCancel(%s.Background())\n", ctx, ctxPkg, ctxPkg)
	b.WriteString("defer cancel()\n")
	fmt.Fprintf(b, "sigc := make(chan %s.Signal, 2)\n", osp)
	fmt.Fprintf(b, "%s.Notify(sigc, %s.Interrupt, %s.SIGTERM)\n", sigp, osp, sysp)
	fmt.Fprintf(b, "defer %s.Stop(sigc)\n", sigp)
	fmt.Fprintf(b, "go func() {\n<-sigc\ncancel()\n<-sigc\n%s.Exit(1)\n}()\n\n", osp)

	wrote := false
	for _, idx := range g.treeOrder {
		var inputs []CommandInput
		switch {
		case idx > 0:
			inputs = g.commandFuncs[idx-1].Inputs
		case idx < 0:
			inputs = g.commandGroups[-idx-1].Inputs
		default:
			inputs = g.commandRoot.Inputs
		}
		for _, in := range inputs {
			fmt.Fprintf(b, "%s := %s\n", in.Var, in.Builder)
			wrote = true
		}
	}
	if wrote {
		b.WriteString("\n")
	}
	fmt.Fprintf(b, "root := &%s.Command{\n", clip)
	fmt.Fprintf(b, "Name: %q,\n", name)
	if r := g.commandRoot; r != nil {
		if r.Usage != "" {
			fmt.Fprintf(b, "Usage: %q,\n", r.Usage)
		}
		if r.Version != "" {
			fmt.Fprintf(b, "Version: %q,\n", r.Version)
		}
		if r.Long != "" {
			fmt.Fprintf(b, "Long: %q,\n", r.Long)
		}
		writeInputFields(b, clip, r.Inputs, r.Use, r.Examples)
	}
	fmt.Fprintf(b, "Subcommands: []*%s.Command{\n", clip)
	for _, n := range buildCommandTree(g.commandFuncs, g.commandGroups, g.treeOrder) {
		g.writeCommandNode(b, clip, n)
	}
	b.WriteString("},\n}\n")
	fmt.Fprintf(b, "return root.Exec(%s.Args[1:], %s.WithSignalContext(%s))\n", osp, clip, ctx)
	b.WriteString("}\n\n")
}

type commandNode struct {
	name     string
	cmd      *CommandFunc
	group    *CommandGroup
	children []*commandNode
}

// buildCommandTree merges paths and preserves first-contribution sibling order.
func buildCommandTree(cs []CommandFunc, groups []CommandGroup, order []int) []*commandNode {
	root := &commandNode{}
	at := func(path []string) *commandNode {
		n := root
		for _, seg := range path {
			var child *commandNode
			for _, existing := range n.children {
				if existing.name == seg {
					child = existing
					break
				}
			}
			if child == nil {
				child = &commandNode{name: seg}
				n.children = append(n.children, child)
			}
			n = child
		}
		return n
	}
	if order == nil {
		for i := range cs {
			order = append(order, i+1)
		}
		for i := range groups {
			order = append(order, -i-1)
		}
	}
	for _, idx := range order {
		if idx == 0 {
			continue
		}
		if idx > 0 {
			c := &cs[idx-1]
			path := c.Path
			if len(path) == 0 {
				path = []string{c.Name}
			}
			at(path).cmd = c
		} else {
			cg := &groups[-idx-1]
			at(cg.Path).group = cg
		}
	}
	return root.children
}

func (g *Gen) writeCommandNode(b *bytes.Buffer, clip string, n *commandNode) {
	b.WriteString("{\n")
	fmt.Fprintf(b, "Name: %q,\n", n.name)
	if c := n.cmd; c != nil {
		if c.Help != "" {
			fmt.Fprintf(b, "Help: %q,\n", c.Help)
		}
		if c.Long != "" {
			fmt.Fprintf(b, "Long: %q,\n", c.Long)
		}
		writeNodeMeta(b, c.Usage, c.Aliases, c.Hidden)
		writeCommandInputs(b, clip, *c)
	} else if cg := n.group; cg != nil {
		if cg.Help != "" {
			fmt.Fprintf(b, "Help: %q,\n", cg.Help)
		}
		if cg.Long != "" {
			fmt.Fprintf(b, "Long: %q,\n", cg.Long)
		}
		writeNodeMeta(b, cg.Usage, cg.Aliases, cg.Hidden)
		writeInputFields(b, clip, cg.Inputs, cg.Use, cg.Examples)
	}
	if len(n.children) > 0 {
		fmt.Fprintf(b, "Subcommands: []*%s.Command{\n", clip)
		for _, child := range n.children {
			g.writeCommandNode(b, clip, child)
		}
		b.WriteString("},\n")
	}
	if c := n.cmd; c != nil {
		fmt.Fprintf(b, "Run: func(ctx %s.Context) error {\n", clip)
		g.writeCommandWrapper(b, *c)
		b.WriteString("},\n")
	}
	b.WriteString("},\n")
}

func (g *Gen) writeCommandWrapper(b *bytes.Buffer, c CommandFunc) {
	s := c.Scope
	if len(s.nodes) == 0 {
		args := append([]string{"ctx"}, s.rootExprs...)
		args = append(args, c.ValueExprs...)
		fmt.Fprintf(b, "return %s(%s)\n", c.Fn, strings.Join(args, ", "))
		return
	}
	vars := wrapperVars(g, s)
	lhs := append([]string{}, vars...)
	if s.hasCleanup {
		lhs = append(lhs, "cleanup")
	}
	if len(lhs) == 0 {
		fmt.Fprintf(b, "if err := %s(ctx); err != nil {\nreturn err\n}\n", s.fn)
	} else {
		fmt.Fprintf(b, "%s, err := %s(ctx)\n", strings.Join(lhs, ", "), s.fn)
		b.WriteString("if err != nil {\nreturn err\n}\n")
		if s.hasCleanup {
			b.WriteString("defer cleanup()\n")
		}
	}
	args := append([]string{"ctx"}, vars...)
	args = append(args, c.ValueExprs...)
	fmt.Fprintf(b, "return %s(%s)\n", c.Fn, strings.Join(args, ", "))
}

func writeCommandInputs(b *bytes.Buffer, clip string, c CommandFunc) {
	writeInputFields(b, clip, c.Inputs, c.Use, c.Examples)
}

func writeNodeMeta(b *bytes.Buffer, usage string, aliases []string, hidden bool) {
	if usage != "" {
		fmt.Fprintf(b, "Usage: %q,\n", usage)
	}
	if len(aliases) > 0 {
		quoted := make([]string, len(aliases))
		for i, a := range aliases {
			quoted[i] = fmt.Sprintf("%q", a)
		}
		fmt.Fprintf(b, "Aliases: []string{%s},\n", strings.Join(quoted, ", "))
	}
	if hidden {
		b.WriteString("Hidden: true,\n")
	}
}

func writeInputFields(b *bytes.Buffer, clip string, inputs []CommandInput, use []string, examples []CommandExample) {
	var flags, cliargs []string
	for _, in := range inputs {
		if in.Arg {
			cliargs = append(cliargs, in.Var)
		} else {
			flags = append(flags, in.Var)
		}
	}
	if len(flags) > 0 {
		fmt.Fprintf(b, "Flags: %s.Flags(%s),\n", clip, strings.Join(flags, ", "))
	}
	if len(cliargs) > 0 {
		fmt.Fprintf(b, "Args: %s.Args(%s),\n", clip, strings.Join(cliargs, ", "))
	}
	if len(use) > 0 {
		fmt.Fprintf(b, "Use: []%s.Middleware{%s},\n", clip, strings.Join(use, ", "))
	}
	if len(examples) > 0 {
		fmt.Fprintf(b, "Examples: []%s.Example{\n", clip)
		for _, e := range examples {
			if e.Help != "" {
				fmt.Fprintf(b, "{Cmd: %q, Help: %q},\n", e.Cmd, e.Help)
			} else {
				fmt.Fprintf(b, "{Cmd: %q},\n", e.Cmd)
			}
		}
		b.WriteString("},\n")
	}
}

// wrapperVars avoids locals that shadow imported packages.
func wrapperVars(g *Gen, s *Scope) []string {
	taken := map[string]bool{"ctx": true, "cleanup": true, "err": true}
	for a := range g.aliasIdents {
		taken[a] = true
	}
	out := make([]string, 0, len(s.roots))
	for _, r := range s.roots {
		base := depVarBase(r)
		name := base
		for n := 2; taken[name]; n++ {
			name = fmt.Sprintf("%s%d", base, n)
		}
		taken[name] = true
		out = append(out, name)
	}
	return out
}

// depVarBase preserves initialisms: Server -> server, DB -> db, HTTPConfig -> httpConfig.
func depVarBase(t types.Type) string {
	tt := types.Unalias(t)
	if p, ok := tt.(*types.Pointer); ok {
		tt = types.Unalias(p.Elem())
	}
	name := "dep"
	if n, ok := tt.(*types.Named); ok {
		name = n.Obj().Name()
	}
	if !strings.ContainsFunc(name, unicode.IsLower) {
		return strings.ToLower(name)
	}
	rs := []rune(name)
	upper := 0
	for upper < len(rs) && unicode.IsUpper(rs[upper]) {
		upper++
	}
	switch {
	case upper == len(rs):
		return strings.ToLower(name)
	case upper > 1:
		return strings.ToLower(string(rs[:upper-1])) + string(rs[upper-1:])
	default:
		return LowerFirst(name)
	}
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
