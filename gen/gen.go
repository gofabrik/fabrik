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
	PhaseInit       Phase = iota // setup calls that run before everything
	PhaseWire                    // construct providers and framework instances
	PhaseMiddleware              // global middleware registration
	PhaseRegister                // register routes and handlers
	PhaseServe                   // start serving; must end with a return
)

var phaseLabels = []struct {
	phase Phase
	label string
}{
	{PhaseInit, "Init"},
	{PhaseWire, "Providers"},
	{PhaseMiddleware, "Middleware"},
	{PhaseRegister, "Routes"},
	{PhaseServe, "Serve"},
}

type stmtRecord struct {
	phase     Phase
	text      string
	directive string
}

// lazyBind emits a provider only when another directive resolves it.
type lazyBind struct {
	build   func() (string, diag.Diagnostics)
	running bool
}

// Gen assembles main.gen.go from imports, DI bindings, and phased statements.
type Gen struct {
	imports    map[string]string // import path -> alias
	idents     map[string]bool   // taken identifiers: aliases and vars
	binds      typeutil.Map      // types.Type -> map[string]string (name -> expr)
	lazy       typeutil.Map      // types.Type -> map[string]*lazyBind
	singletons map[string]string // singleton key -> var name
	stmts      []stmtRecord
	current    string // directive name, for provenance

	materializing []string // active lazy-bind stack
}

// New returns an empty Gen.
func New() *Gen {
	return &Gen{
		imports:    map[string]string{},
		idents:     map[string]bool{},
		singletons: map[string]string{},
	}
}

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
	m[name] = &lazyBind{build: build}
}

// Instance resolves the wired expression for (t, name).
// Failed resolutions with diagnostics are complete diagnostics.
func (g *Gen) Instance(t types.Type, name string) (string, diag.Diagnostics, bool) {
	t = types.Unalias(t)
	if m, _ := g.binds.At(t).(map[string]string); m != nil {
		if expr, ok := m[name]; ok {
			return expr, nil, true
		}
	}
	if m, _ := g.lazy.At(t).(map[string]*lazyBind); m != nil {
		if lb, ok := m[name]; ok {
			key := g.TypeExpr(t)
			if lb.running {
				return "", diag.Diagnostics{g.cycleDiag(key)}, false
			}
			lb.running = true
			g.materializing = append(g.materializing, key)
			expr, ds := lb.build()
			g.materializing = g.materializing[:len(g.materializing)-1]
			g.Bind(t, name, expr)
			return expr, ds, true
		}
	}
	return "", nil, false
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
	g.Stmt(phase, "%s := %s", v, ctor)
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
	return false
}

// HasSingleton reports whether key has been created.
func (g *Gen) HasSingleton(key string) bool {
	_, ok := g.singletons[key]
	return ok
}

// Stmt appends one statement to a phase.
func (g *Gen) Stmt(phase Phase, format string, a ...any) {
	g.stmts = append(g.stmts, stmtRecord{phase: phase, text: fmt.Sprintf(format, a...), directive: g.current})
}

// Render assembles and formats main.gen.go.
func (g *Gen) Render() ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("// Code generated by fabrik. DO NOT EDIT.\n\npackage main\n\n")
	g.writeImports(&b)
	b.WriteString("func run() error {\n")
	served := false
	first := true
	for _, pl := range phaseLabels {
		wrote := false
		for _, s := range g.stmts {
			if s.phase != pl.phase {
				continue
			}
			if !wrote {
				if !first {
					b.WriteString("\n")
				}
				first = false
				b.WriteString("// " + pl.label + "\n")
			}
			wrote = true
			b.WriteString(s.text)
			b.WriteString("\n")
		}
		if pl.phase == PhaseServe && wrote {
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
		if s := g.findUnparsable(); s != nil {
			return nil, fmt.Errorf("directive %q emitted an unparsable statement:\n%s", s.directive, s.text)
		}
		return nil, fmt.Errorf("format generated source: %w\n%s", err, b.String())
	}
	return src, nil
}

func (g *Gen) writeImports(b *bytes.Buffer) {
	if len(g.imports) == 0 {
		return
	}
	paths := make([]string, 0, len(g.imports))
	for p := range g.imports {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var std, mod []string
	for _, p := range paths {
		if line := g.importLine(p); strings.Contains(strings.SplitN(p, "/", 2)[0], ".") {
			mod = append(mod, line)
		} else {
			std = append(std, line)
		}
	}
	b.WriteString("import (\n")
	b.WriteString(strings.Join(std, "\n"))
	if len(std) > 0 && len(mod) > 0 {
		b.WriteString("\n\n")
	}
	b.WriteString(strings.Join(mod, "\n"))
	b.WriteString("\n)\n\n")
}

func (g *Gen) importLine(path string) string {
	alias := g.imports[path]
	if i := strings.LastIndexByte(path, '/'); path[i+1:] == alias {
		return fmt.Sprintf("%q", path)
	}
	return fmt.Sprintf("%s %q", alias, path)
}

// findUnparsable locates the first statement that fails to parse on its own.
func (g *Gen) findUnparsable() *stmtRecord {
	for i, s := range g.stmts {
		probe := "package p\nfunc _() error {\n" + s.text + "\nreturn nil\n}\n"
		if _, err := format.Source([]byte(probe)); err != nil {
			return &g.stmts[i]
		}
	}
	return nil
}
