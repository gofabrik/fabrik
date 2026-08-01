package gen

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
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
)

var phaseLabels = []struct {
	phase Phase
	label string
}{
	{PhaseConfig, "Config"},
	{PhaseSetup, "Wire hooks: config exists, providers do not"},
	{PhaseWire, "Providers"},
	{PhaseMiddleware, "Middleware"},
	{PhaseRegister, "Register"},
}

type lazyBind struct {
	build   func() (string, diag.Diagnostics)
	owner   string
	pos     token.Position
	running bool

	// A build that returned fatal diagnostics is cached so later flows
	// reuse its expression without re-running or re-reporting.
	diagnosed     bool
	diagnosedExpr string
}

// Gen assembles main.gen.go from imports, DI bindings, and phased statements.
type Gen struct {
	imports     map[string]string // import path -> alias
	idents      map[string]bool   // taken identifiers: aliases and vars
	binds       typeutil.Map      // types.Type -> map[string]string (name -> expr)
	bindOrigins typeutil.Map      // types.Type -> map[string]string (name -> owner)
	lazy        typeutil.Map      // types.Type -> map[string]*lazyBind
	lazyByPath  map[string]*lazyBind
	pathExprs   map[string]string // materialized path bindings, for InstancePath
	singletons  map[string]string // singleton key -> var name
	onceVals    map[string]string // OnceValue cache
	ctxNeeded   bool              // context.Background assignment is needed
	ctxPkg      string            // context import alias, registered at request time
	ctxVar      string            // reserved context identifier, usually "ctx"
	current     string            // active directive
	module      string            // module path of the generated app
	hints       []func(types.Type) (string, bool)
	types       map[string]*types.Package // import path -> package, for LookupType

	materializing []string // active lazy-bind stack

	commandFuncs  []CommandFunc
	commandGroups []CommandGroup
	commandRoot   *RootSpec
	// treeOrder preserves arrival order: commands use positive indices, groups negative, and root zero.
	treeOrder []int
	prologues []func() diag.Diagnostics
	epilogues []func() diag.Diagnostics

	scopes      []*Scope
	scope       *Scope          // active scope; nil uses default state
	aliasIdents map[string]bool // import aliases reserved by new scopes

	inject map[injectKey]*injectEntry

	comments CommentLevel
	srcRoot  string
	buildTag string

	bindOrder []string // First appearance determines graph binding ordinals.
	bindSeen  map[string]bool

	bindConflicts diag.Diagnostics
	conflictSeen  map[string]bool // survives BindConflicts drains
	rendered      bool

	demands         map[demandKey]map[string]bool
	demandEdges     map[demandKey]map[demandKey]bool
	callbackSeen    map[string]bool // epilogue diagnostics already reported
	frag            *fragmentStore  // non-nil in fragment emission mode
	fragPlan        *regionPlan
	varTypes        map[string]varInfo
	regionConsumers map[string][]regionConsumer
	outputIdents    map[string]bool // output-package declarations

	batchID  string // active order-sensitive emission batch
	batchSeq int

	embedded  bool
	outputPkg string
}

// New returns an empty Gen.
func New() *Gen {
	return &Gen{
		imports:     map[string]string{},
		idents:      map[string]bool{},
		lazyByPath:  map[string]*lazyBind{},
		pathExprs:   map[string]string{},
		singletons:  map[string]string{},
		onceVals:    map[string]string{},
		aliasIdents: map[string]bool{},
		bindSeen:    map[string]bool{},
		frag:        &fragmentStore{},
	}
}

// recordBindExpr fixes graph binding ordinals at first bind.
func (g *Gen) recordBindExpr(expr string) {
	if sc := g.scope; sc != nil {
		if !sc.bindSeen[expr] {
			sc.bindSeen[expr] = true
			sc.bindOrder = append(sc.bindOrder, expr)
		}
		return
	}
	if !g.bindSeen[expr] {
		g.bindSeen[expr] = true
		g.bindOrder = append(g.bindOrder, expr)
	}
}

// SetModule records the module path of the app being generated.
func (g *Gen) SetModule(path string) { g.module = path }

// CommentLevel controls generated comments; the zero value emits sections.
type CommentLevel int

const (
	// CommentsSections emits the phase section headers and node labels.
	CommentsSections CommentLevel = iota
	// CommentsOff suppresses all generated comments.
	CommentsOff
	// CommentsFull adds a per-node origin line: `// <directive> <pos>`.
	CommentsFull
)

// SetCommentLevel selects the generated comment level.
func (g *Gen) SetCommentLevel(l CommentLevel) { g.comments = l }

// SetSourceRoot makes origin positions relative to dir.
func (g *Gen) SetSourceRoot(dir string) { g.srcRoot = dir }

// EmbeddedOutput switches rendering to entry constructors in the
// named non-main package: no run(), no CLI tree, no signal handling;
// the host owns lifecycle.
func (g *Gen) EmbeddedOutput(pkg string) {
	g.embedded = true
	g.outputPkg = pkg
}

// SetBuildTag constrains the generated file with a //go:build line; empty leaves it unconstrained.
func (g *Gen) SetBuildTag(tag string) { g.buildTag = tag }

func (g *Gen) originComment(n Node) string {
	o := n.base().Origin
	if o.Directive == "" {
		return ""
	}
	if !o.Pos.IsValid() {
		return "// " + o.Directive
	}
	file := o.Pos.Filename
	if g.srcRoot != "" {
		if rel, err := filepath.Rel(g.srcRoot, file); err == nil {
			file = rel
		}
	}
	return fmt.Sprintf("// %s %s:%d", o.Directive, file, o.Pos.Line)
}

func (g *Gen) nodeComments(b *bytes.Buffer, n Node) {
	if g.comments == CommentsFull {
		if c := g.originComment(n); c != "" {
			b.WriteString(c)
			b.WriteString("\n")
		}
	}
	if g.comments == CommentsOff {
		return
	}
	if l := n.base().Label; l != "" {
		b.WriteString("// " + l + "\n")
	}
}

func (g *Gen) nodeLines(n Node, ec *errCtx) []string {
	if g.comments == CommentsFull {
		if sel, ok := n.(*Select); ok {
			return renderSelectWithComments(sel, ec, g.originComment)
		}
	}
	return renderNode(n, ec)
}

// SetTypes supplies type-checked packages for [Gen.LookupType].
func (g *Gen) SetTypes(m map[string]*types.Package) { g.types = m }

// ReserveOutputIdents reserves identifiers declared in the output
// package so generated declarations never collide with them.
func (g *Gen) ReserveOutputIdents(names []string) {
	if g.outputIdents == nil {
		g.outputIdents = map[string]bool{}
	}
	for _, n := range names {
		g.outputIdents[n] = true
	}
}

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
	if _, after, found := strings.CutLast(base, "/"); found {
		base = after
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
		for n := 2; sc.idents[name] || sc.reserved[name]; n++ {
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

// TypeDisplay renders t with existing aliases without registering imports.
func (g *Gen) TypeDisplay(t types.Type) string {
	return types.TypeString(t, func(p *types.Package) string {
		if alias, ok := g.imports[p.Path()]; ok {
			return alias
		}
		return p.Name()
	})
}

// Bind records expr as the wired value for (t, name).
func (g *Gen) Bind(t types.Type, name, expr string) {
	g.checkBindRegistration()
	t = types.Unalias(t)
	owner := g.current
	if sc := g.scope; sc != nil {
		key := types.TypeString(t, nil)
		m := sc.binds[key]
		if m == nil {
			m = map[string]string{}
			sc.binds[key] = m
			sc.bindTypes[key] = t
		}
		if sc.bindOrigins[key] == nil {
			sc.bindOrigins[key] = map[string]string{}
		}
		if _, dup := m[name]; dup {
			g.recordBindConflict(token.Position{}, fmt.Sprintf(
				"duplicate bind for %s %q: directive %q conflicts with directive %q",
				t, name, owner, sc.bindOrigins[key][name],
			), token.Position{})
			return
		}
		m[name] = expr
		sc.bindOrigins[key][name] = owner
		g.noteVarType(expr, t)
		g.recordBindExpr(expr)
		return
	}
	m, _ := g.binds.At(t).(map[string]string)
	if m == nil {
		m = map[string]string{}
		g.binds.Set(t, m)
	}
	origins, _ := g.bindOrigins.At(t).(map[string]string)
	if origins == nil {
		origins = map[string]string{}
		g.bindOrigins.Set(t, origins)
	}
	if _, dup := m[name]; dup {
		g.recordBindConflict(token.Position{}, fmt.Sprintf(
			"duplicate bind for %s %q: directive %q conflicts with directive %q",
			t, name, owner, origins[name],
		), token.Position{})
		return
	}
	m[name] = expr
	origins[name] = owner
	g.noteVarType(expr, t)
	g.recordBindExpr(expr)
}

// BindConflicts returns and clears binding collisions accumulated during registration.
func (g *Gen) BindConflicts() diag.Diagnostics {
	ds := g.bindConflicts
	g.bindConflicts = nil
	return ds
}

func (g *Gen) recordBindConflict(pos token.Position, message string, first token.Position) {
	help := ""
	if first.IsValid() {
		help = fmt.Sprintf("first declared at %s", first)
	}
	// A build re-run by another scope replays the same registration;
	// one logical conflict reports once, across drains. A different
	// declaration differs in position or help and stays distinct.
	key := message + "\x00" + pos.String() + "\x00" + help
	if g.conflictSeen[key] {
		return
	}
	if g.conflictSeen == nil {
		g.conflictSeen = map[string]bool{}
	}
	g.conflictSeen[key] = true
	g.bindConflicts.Error(pos, message, help)
}

func (g *Gen) checkBindRegistration() {
	if g.rendered {
		panic("gen: binding registered after Render")
	}
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
	g.BindLazyAt(t, name, token.Position{}, build)
}

// BindLazyAt is BindLazy carrying the registering declaration's
// position for collision diagnostics.
func (g *Gen) BindLazyAt(t types.Type, name string, pos token.Position, build func() (string, diag.Diagnostics)) {
	g.checkBindRegistration()
	t = types.Unalias(t)
	m, _ := g.lazy.At(t).(map[string]*lazyBind)
	if m == nil {
		m = map[string]*lazyBind{}
		g.lazy.Set(t, m)
	}
	if first, dup := m[name]; dup {
		g.recordBindConflict(pos, fmt.Sprintf(
			"duplicate lazy bind for %s %q: directive %q conflicts with directive %q",
			t, name, g.current, first.owner,
		), first.pos)
		return
	}
	m[name] = &lazyBind{build: build, owner: g.current, pos: pos}
}

// BindPath publishes an expression while its lazy path binding is materializing.
func (g *Gen) BindPath(path, expr string) {
	g.checkBindRegistration()
	g.noteVarPathType(expr, path)
	if sc := g.scope; sc != nil {
		sc.pathExprs[path] = expr
		g.recordBindExpr(expr)
		return
	}
	g.pathExprs[path] = expr
	g.recordBindExpr(expr)
}

// BindLazyPath registers a lazy binding matched by a printed type path.
func (g *Gen) BindLazyPath(path string, build func() (string, diag.Diagnostics)) {
	g.BindLazyPathAt(path, token.Position{}, build)
}

// BindLazyPathAt is BindLazyPath with the registering annotation's
// position; positionless emissions during the build inherit it in
// diagnostics.
func (g *Gen) BindLazyPathAt(path string, pos token.Position, build func() (string, diag.Diagnostics)) {
	g.checkBindRegistration()
	if first, dup := g.lazyByPath[path]; dup {
		g.recordBindConflict(pos, fmt.Sprintf(
			"duplicate lazy bind for path %s: directive %q conflicts with directive %q",
			path, g.current, first.owner,
		), first.pos)
		return
	}
	g.lazyByPath[path] = &lazyBind{build: build, owner: g.current, pos: pos}
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
				expr, ds, ok := g.materialize(t, name, lb, demandKey{kind: demandPath, key: key})
				if ok && len(ds) == 0 {
					sc.pathExprs[key] = expr
					g.recordBindExpr(expr)
				}
				return expr, ds, ok
			}
		}
		if m, _ := g.lazy.At(t).(map[string]*lazyBind); m != nil {
			if lb, ok := m[name]; ok {
				return g.materialize(t, name, lb, demandKey{kind: demandType, key: key, name: name})
			}
		}
		return "", nil, false
	}
	if m, _ := g.binds.At(t).(map[string]string); m != nil {
		if expr, ok := m[name]; ok {
			key := types.TypeString(t, nil)
			if lm, _ := g.lazy.At(t).(map[string]*lazyBind); lm != nil {
				if _, lazyDef := lm[name]; lazyDef {
					g.recordDemand(demandKey{kind: demandType, key: key, name: name})
				}
			}
			// A path-bound value reached through the typed cache still
			// accumulates its path demand.
			if name == "" {
				if _, pathDef := g.lazyByPath[key]; pathDef {
					g.recordDemand(demandKey{kind: demandPath, key: key})
				}
			}
			return expr, nil, true
		}
	}
	if path := types.TypeString(t, nil); name == "" {
		if expr, ok := g.pathExprs[path]; ok {
			g.recordDemand(demandKey{kind: demandPath, key: path})
			// Bind the concrete type on first type-keyed lookup.
			g.Bind(t, name, expr)
			return expr, nil, true
		}
		if lb, ok := g.lazyByPath[path]; ok {
			expr, ds, ok := g.materialize(t, name, lb, demandKey{kind: demandPath, key: path})
			if ok && len(ds) == 0 {
				g.pathExprs[path] = expr
				g.recordBindExpr(expr)
			}
			return expr, ds, ok
		}
	}
	if m, _ := g.lazy.At(t).(map[string]*lazyBind); m != nil {
		if lb, ok := m[name]; ok {
			return g.materialize(t, name, lb, demandKey{kind: demandType, key: types.TypeString(t, nil), name: name})
		}
	}
	return "", nil, false
}

// HasBindingPath reports whether a path-keyed binding exists, lazy or
// already materialized.
func (g *Gen) HasBindingPath(path string) bool {
	exprs := g.pathExprs
	if sc := g.scope; sc != nil {
		exprs = sc.pathExprs
	}
	if _, ok := exprs[path]; ok {
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
		g.recordDemand(demandKey{kind: demandPath, key: path})
		return expr, nil, true
	}
	return g.materializePath(path)
}

func (g *Gen) materializePath(path string) (expr string, ds diag.Diagnostics, ok bool) {
	lb, ok := g.lazyByPath[path]
	if !ok {
		return "", nil, false
	}
	g.recordDemand(demandKey{kind: demandPath, key: path})
	if lb.diagnosed {
		return lb.diagnosedExpr, nil, true
	}
	if g.lazyRunning(lb) {
		return "", diag.Diagnostics{g.cycleDiag(path)}, false
	}
	popDemand := g.pushDemand(demandKey{kind: demandPath, key: path})
	defer popDemand()
	popPos := g.pushNodePos(lb.pos)
	defer popPos()
	g.setLazyRunning(lb, true)
	g.materializing = append(g.materializing, path)
	prev := g.current
	g.current = lb.owner
	defer func() {
		g.current = prev
		g.materializing = g.materializing[:len(g.materializing)-1]
		g.setLazyRunning(lb, false)
	}()
	defer func() {
		if p := recover(); p != nil {
			expr = ""
			ok = false
			ds.Error(token.Position{}, lazyPanicMessage(lb.owner, p), "")
			if !g.InValidationScope() {
				lb.diagnosed = true
				lb.diagnosedExpr = ""
			}
		}
	}()
	expr, ds = lb.build()
	// Flow walks deduplicate diagnostics across flows; direct calls remain retryable.
	if ds.HasFatal() && !g.InValidationScope() {
		lb.diagnosed = true
		lb.diagnosedExpr = expr
	}
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

func (g *Gen) materialize(t types.Type, name string, lb *lazyBind, dk demandKey) (expr string, ds diag.Diagnostics, ok bool) {
	if lb.diagnosed {
		// Populate the bind cache for consumers after the initial diagnostic.
		if !g.typeBound(t, name) {
			g.Bind(t, name, lb.diagnosedExpr)
		}
		return lb.diagnosedExpr, nil, true
	}
	// Avoid adding imports while formatting cycle diagnostics.
	key := types.TypeString(t, func(p *types.Package) string { return p.Name() })
	chainKey := key
	if name != "" {
		chainKey += " (" + name + ")"
	}
	if g.lazyRunning(lb) {
		return "", diag.Diagnostics{g.cycleDiag(chainKey)}, false
	}
	g.setLazyRunning(lb, true)
	g.materializing = append(g.materializing, chainKey)
	prev := g.current
	g.current = lb.owner
	g.recordDemand(dk)
	popDemand := g.pushDemand(dk)
	popPos := g.pushNodePos(lb.pos)
	defer func() {
		popPos()
		popDemand()
		g.current = prev
		g.materializing = g.materializing[:len(g.materializing)-1]
		g.setLazyRunning(lb, false)
	}()
	defer func() {
		if p := recover(); p != nil {
			expr = ""
			ok = false
			ds.Error(token.Position{}, lazyPanicMessage(lb.owner, p), "")
			// Deduplicate panics across scope passes while leaving direct calls retryable.
			if !g.InValidationScope() {
				lb.diagnosed = true
				lb.diagnosedExpr = ""
			}
		}
	}()
	expr, ds = lb.build()
	if ds.HasFatal() && !g.InValidationScope() {
		lb.diagnosed = true
		lb.diagnosedExpr = expr
	}
	// BindPath may populate the type-keyed cache during the build.
	if !g.typeBound(t, name) {
		g.Bind(t, name, expr)
	}
	return expr, ds, true
}

func lazyPanicMessage(owner string, p any) string {
	if owner == "" {
		return fmt.Sprintf("internal error: lazy binding panicked during materialization: %v", p)
	}
	return fmt.Sprintf("internal error: directive %q panicked during lazy binding materialization: %v", owner, p)
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

// BeginBatch preserves the order of subsequent emissions until the returned function is called.
func (g *Gen) BeginBatch(id string) func() {
	prevID, prevSeq := g.batchID, g.batchSeq
	g.batchID = id
	g.batchSeq = 0
	return func() { g.batchID, g.batchSeq = prevID, prevSeq }
}

// OnceValue runs build once per key in the active scope and caches its result.
func (g *Gen) OnceValue(key string, build func() string) string {
	if sc := g.scope; sc != nil {
		if v, ok := sc.onceVals[key]; ok {
			return v
		}
		v := build()
		sc.onceVals[key] = v
		return v
	}
	if v, ok := g.onceVals[key]; ok {
		return v
	}
	v := build()
	g.onceVals[key] = v
	return v
}

// Singleton returns the shared variable for key, emitting it on first use.
func (g *Gen) Singleton(key, varName, ctor string) string {
	return g.SingletonIn(PhaseWire, key, varName, ctor)
}

// SingletonIn emits one variable per key in the default flow or active scope.
func (g *Gen) SingletonIn(phase Phase, key, varName, ctor string) string {
	g.recordDemand(demandKey{kind: demandSingleton, key: key})
	done := g.pushDemand(demandKey{kind: demandSingleton, key: key})
	defer done()
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
	if sc := g.scope; sc != nil {
		key := types.TypeString(t, nil)
		if m := sc.binds[key]; m != nil {
			if _, ok := m[name]; ok {
				return true
			}
		}
		if name == "" {
			if _, ok := sc.pathExprs[key]; ok {
				return true
			}
			if _, ok := g.lazyByPath[key]; ok {
				return true
			}
		}
		if m, _ := g.lazy.At(t).(map[string]*lazyBind); m != nil {
			if _, ok := m[name]; ok {
				return true
			}
		}
		return false
	}
	if m, _ := g.binds.At(t).(map[string]string); m != nil {
		if _, ok := m[name]; ok {
			return true
		}
	}
	if path := types.TypeString(t, nil); name == "" {
		if _, ok := g.pathExprs[path]; ok {
			return true
		}
		if _, ok := g.lazyByPath[path]; ok {
			return true
		}
	}
	if m, _ := g.lazy.At(t).(map[string]*lazyBind); m != nil {
		if _, ok := m[name]; ok {
			return true
		}
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

// Render assembles and formats main.gen.go.
func (g *Gen) Render() ([]byte, error) {
	g.rendered = true
	if err := validateCommandMetadata(g.commandFuncs, g.commandGroups); err != nil {
		return nil, err
	}
	for _, s := range g.scopes {
		if err := validateExprFields(s.nodes); err != nil {
			return nil, err
		}
	}
	storeNodes := make([]Node, 0, len(g.frag.nodes))
	for _, sn := range g.frag.nodes {
		storeNodes = append(storeNodes, sn.n)
	}
	if err := validateExprFields(storeNodes); err != nil {
		return nil, err
	}
	hasCmd := len(g.commandFuncs) > 0 || len(g.commandGroups) > 0 || g.commandRoot != nil
	if hasCmd || g.embedded {
		g.Context()
	}
	runNodes := g.defaultInlineNodes()
	needsCtx := hasCmd || g.embedded || usesCtx(runNodes, g.ctxVar)

	// Imports must be registered before the import block is written.
	// lateAliases must include every alias registered here.
	ctxPkg := ""
	if needsCtx {
		ctxPkg = g.Import("context")
	}
	if g.embedded {
		if g.entrypointsNeedErrors() {
			g.Import("errors")
		}
	} else {
		g.Import("os")
		if hasCmd {
			g.Import("os/signal")
			g.Import("syscall")
			g.Import("github.com/gofabrik/fabrik/cli")
		} else {
			g.Import("fmt")
		}
	}

	if len(g.scopes) > 0 {
		g.Import("context")
	}

	var body bytes.Buffer
	if g.embedded {
		g.writeEntrypoints(&body, ctxPkg)
	} else {
		g.writeRun(&body, needsCtx, ctxPkg)
	}
	g.writeRegionFuncs(&body)

	var used map[string]bool
	if g.embedded {
		// Embedded output drops the CLI shell, so imports registered for
		// it prune against what the entries actually reference.
		u, err := usedIdents(body.String())
		if err != nil {
			return nil, err
		}
		used = u
	}
	var b bytes.Buffer
	g.writeFileHeader(&b)
	g.writeImports(&b, used)
	b.Write(body.Bytes())

	return g.formatFile(b.Bytes())
}

func (g *Gen) writeFileHeader(b *bytes.Buffer) {
	if g.buildTag != "" {
		b.WriteString("//go:build " + g.buildTag + "\n\n")
	}
	pkg := g.outputPkg
	if pkg == "" {
		pkg = "main"
	}
	b.WriteString("// Code generated by fabrik. DO NOT EDIT.\n// Regenerate with: fabrik wire\n\npackage " + pkg + "\n\n")
}

func (g *Gen) formatFile(src []byte) ([]byte, error) {
	out, err := format.Source(src)
	if err != nil {
		if directive, text, found := g.findUnparsable(); found {
			return nil, fmt.Errorf("directive %q emitted an unparsable statement:\n%s", directive, text)
		}
		return nil, fmt.Errorf("format generated source: %w\n%s", err, src)
	}
	return out, nil
}

// RenderFiles renders the main file and one import-pruned file per extracted region.
// Outside fragment mode it returns only the main file.
func (g *Gen) RenderFiles() (map[string][]byte, error) {
	single, err := g.Render()
	if err != nil {
		return nil, err
	}
	if g.fragPlan == nil || len(g.fragPlan.regions) == 0 {
		return map[string][]byte{g.mainFileName(): single}, nil
	}
	hasCmd := len(g.commandFuncs) > 0 || len(g.commandGroups) > 0 || g.commandRoot != nil
	runNodes := g.defaultInlineNodes()
	needsCtx := hasCmd || usesCtx(runNodes, g.ctxVar)
	ctxPkg := g.imports["context"]

	files := map[string][]byte{}
	emit := func(name string, body func(*bytes.Buffer)) error {
		var raw bytes.Buffer
		body(&raw)
		used, err := usedIdents(raw.String())
		if err != nil {
			return fmt.Errorf("split %s: %w", name, err)
		}
		var b bytes.Buffer
		g.writeFileHeader(&b)
		g.writeImports(&b, used)
		b.Write(raw.Bytes())
		out, err := g.formatFile(b.Bytes())
		if err != nil {
			return err
		}
		files[name] = out
		return nil
	}
	if err := emit(g.mainFileName(), func(b *bytes.Buffer) {
		if g.embedded {
			g.writeEntrypoints(b, ctxPkg)
		} else {
			g.writeRun(b, needsCtx, ctxPkg)
		}
	}); err != nil {
		return nil, err
	}
	taken := map[string]bool{g.mainFileName(): true}
	for _, reg := range g.regionEmitOrder() {
		base := "fragments_" + strings.ToLower(reg.fn)
		name := base + ".gen.go"
		for n := 2; taken[name]; n++ {
			name = fmt.Sprintf("%s%d.gen.go", base, n)
		}
		taken[name] = true
		if err := emit(name, func(b *bytes.Buffer) {
			g.writeRegionFunc(b, reg)
		}); err != nil {
			return nil, err
		}
	}
	return files, nil
}

// usedIdents returns identifiers used as package qualifiers for import pruning.
func usedIdents(body string) (map[string]bool, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "split.go", "package main\n\n"+body, 0)
	if err != nil {
		return nil, err
	}
	used := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok {
				used[id.Name] = true
			}
		}
		return true
	})
	return used, nil
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
	b.WriteString("if err := func() (err error) {\n")
	nodes := g.defaultInlineNodes()
	g.emitPhaseNodes(b, nodes, PhaseConfig, PhaseSetup, PhaseWire, PhaseMiddleware, PhaseRegister)
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

func validateCommandMetadata(commands []CommandFunc, groups []CommandGroup) error {
	executable := make(map[string][]string, len(commands))
	for _, command := range commands {
		commandPath := command.Path
		if len(commandPath) == 0 {
			commandPath = []string{command.Name}
		}
		executable[strings.Join(commandPath, "\x00")] = commandPath
	}
	for _, group := range groups {
		if commandPath, ok := executable[strings.Join(group.Path, "\x00")]; ok {
			return fmt.Errorf(
				"gen: CLI path %q has both executable command and group metadata",
				strings.Join(commandPath, " "),
			)
		}
	}
	return nil
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
		fmt.Fprintf(b, "Run: func(ctx %s.Context) (err error) {\n", clip)
		g.writeCommandWrapper(b, *c)
		b.WriteString("},\n")
	}
	b.WriteString("},\n")
}

func (g *Gen) writeCommandWrapper(b *bytes.Buffer, c CommandFunc) {
	g.writeRegionBody(b, c)
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

func appName(module string) string {
	if module == "" {
		return "app"
	}
	if _, after, found := strings.CutLast(module, "/"); found {
		return after
	}
	return module
}

func (g *Gen) emitPhaseNodes(b *bytes.Buffer, allNodes []Node, phases ...Phase) {
	want := map[Phase]bool{}
	for _, p := range phases {
		want[p] = true
	}
	first := true
	lc := newLayoutCtx(allNodes)
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
		if g.comments != CommentsOff {
			b.WriteString("// " + pl.label + "\n")
		}
		clusters := lc.layout(nodes)
		for ci, cluster := range clusters {
			if ci > 0 {
				b.WriteString("\n")
			}
			for _, pn := range cluster {
				g.nodeComments(b, pn.n)
				for _, line := range g.nodeLines(pn.n, nil) {
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
// writeImports emits the import block; a non-nil used set keeps only
// imports whose alias the file references.
func (g *Gen) writeImports(b *bytes.Buffer, used map[string]bool) {
	if len(g.imports) == 0 {
		return
	}
	paths := make([]string, 0, len(g.imports))
	for p := range g.imports {
		if used != nil && !used[g.imports[p]] {
			continue
		}
		paths = append(paths, p)
	}
	if len(paths) == 0 {
		return
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
	base := path
	if _, after, found := strings.CutLast(path, "/"); found {
		base = after
	}
	if base == alias {
		return fmt.Sprintf("%q", path)
	}
	return fmt.Sprintf("%s %q", alias, path)
}

func (g *Gen) findUnparsable() (directive, text string, found bool) {
	storeNodes := make([]Node, 0, len(g.frag.nodes))
	for _, sn := range g.frag.nodes {
		storeNodes = append(storeNodes, sn.n)
	}
	return findUnparsableNodes(storeNodes, nil, "")
}

func findUnparsableNodes(nodes []Node, ec *errCtx, owner string) (directive, text string, found bool) {
	for _, n := range nodes {
		// Bare case results inherit the enclosing directive.
		attr := n.base().Origin.Directive
		if attr == "" {
			attr = owner
		}
		if sel, ok := n.(*Select); ok {
			for _, c := range sel.Cases {
				if directive, text, found := findUnparsableNodes(c.Body, ec, attr); found {
					return directive, text, true
				}
				resultAttr := c.Result.Origin.Directive
				if resultAttr == "" {
					resultAttr = attr
				}
				if directive, text, found := probeLines(renderSelectResult(sel, c, ec), resultAttr); found {
					return directive, text, true
				}
			}
		}
		if directive, text, found := probeLines(renderNode(n, ec), attr); found {
			return directive, text, true
		}
	}
	return "", "", false
}

func probeLines(lines []string, directive string) (string, string, bool) {
	text := strings.Join(lines, "\n")
	probe := "package p\nfunc _() error {\n" + text + "\nreturn nil\n}\n"
	if _, err := format.Source([]byte(probe)); err != nil {
		return directive, text, true
	}
	return "", "", false
}
