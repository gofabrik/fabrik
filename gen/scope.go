package gen

import (
	"bytes"
	"go/token"
	"go/types"
	"sort"
	"strings"

	"github.com/gofabrik/fabrik/diag"
)

// Scope contains the generated state for one command build function.
type Scope struct {
	fn    string
	pos   token.Position
	roots []types.Type

	hasCleanup bool

	idents     map[string]bool
	binds      map[string]map[string]string // printed type -> name -> expr
	pathExprs  map[string]string
	singletons map[string]string
	nodes      []Node
	running    map[*lazyBind]bool
	imports    map[string]string // isolated validation aliases
	ctxVar     string
	validation bool

	rootExprs   []string
	resultTypes []string
	zeros       []string
}

// AddScope registers a build scope for the dependency roots at pos.
func (g *Gen) AddScope(fn string, pos token.Position, roots ...types.Type) *Scope {
	if len(g.scopes) == 0 {
		// Reserve wrapper identifiers before assigning import aliases.
		g.Context()
		g.idents["cleanup"] = true
		g.idents["err"] = true
	}
	s := &Scope{fn: fn, pos: pos, roots: roots}
	g.scopes = append(g.scopes, s)
	return s
}

// ScopeCount reports how many scopes are registered.
func (g *Gen) ScopeCount() int { return len(g.scopes) }

// ScopeID identifies the active scope; nil denotes the default flow.
func (g *Gen) ScopeID() any {
	if g.scope == nil {
		return nil
	}
	return g.scope
}

// InValidationScope reports whether the discarded validation scope is active.
func (g *Gen) InValidationScope() bool {
	return g.scope != nil && g.scope.validation
}

// ScopePrologue registers a callback that runs before dependency resolution in every scope.
func (g *Gen) ScopePrologue(fn func() diag.Diagnostics) {
	g.prologues = append(g.prologues, fn)
}

// ScopeEpilogue registers a callback that runs after dependency resolution in every scope.
func (g *Gen) ScopeEpilogue(fn func() diag.Diagnostics) {
	g.epilogues = append(g.epilogues, fn)
}

func (g *Gen) enterScope(s *Scope, validation bool) {
	s.idents = map[string]bool{}
	for a := range g.aliasIdents {
		s.idents[a] = true
	}
	// Prevent generated locals from shadowing the build function skeleton.
	s.idents["err"] = true
	s.idents["cleanup"] = true
	s.ctxVar = "ctx"
	s.idents["ctx"] = true
	s.binds = map[string]map[string]string{}
	s.pathExprs = map[string]string{}
	s.singletons = map[string]string{}
	s.running = map[*lazyBind]bool{}
	s.validation = validation
	if validation {
		s.imports = map[string]string{}
	}
	g.scope = s
}

// MaterializeScopes resolves each scope after all bindings are registered.
func (g *Gen) MaterializeScopes() diag.Diagnostics {
	var ds diag.Diagnostics
	for _, s := range g.scopes {
		g.enterScope(s, false)
		for _, fn := range g.prologues {
			// Validation already reports prologue diagnostics; replay only emits code.
			fn()
		}
		for _, root := range s.roots {
			expr, rds, ok := g.Instance(root, "")
			ds = append(ds, rds...)
			if !ok && len(rds) == 0 {
				ds.Error(s.pos, "no provider for "+g.TypeExpr(root),
					"add a //fabrik:provider returning "+g.TypeExpr(root))
			}
			s.rootExprs = append(s.rootExprs, expr)
			s.resultTypes = append(s.resultTypes, g.TypeExpr(root))
			s.zeros = append(s.zeros, zeroExpr(g, root))
		}
		for _, fn := range g.epilogues {
			ds = append(ds, fn()...)
		}
		for _, n := range s.nodes {
			if c, ok := n.(*Call); ok && c.Cleanup != "" {
				s.hasCleanup = true
			}
		}
		if s.hasCleanup {
			s.resultTypes = append(s.resultTypes, "func() error")
			s.zeros = append(s.zeros, "nil")
		}
		g.scope = nil
	}
	return ds
}

// RunValidationPass resolves every lazy binding in an isolated scope for diagnostics.
func (g *Gen) RunValidationPass() diag.Diagnostics {
	s := &Scope{}
	g.enterScope(s, true)
	defer func() { g.scope = nil }()

	var ds diag.Diagnostics
	for _, fn := range g.prologues {
		ds = append(ds, fn()...)
	}
	type entry struct {
		key, name string
		t         types.Type
	}
	var entries []entry
	g.lazy.Iterate(func(t types.Type, v any) {
		for name := range v.(map[string]*lazyBind) {
			entries = append(entries, entry{types.TypeString(t, nil), name, t})
		}
	})
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].key != entries[j].key {
			return entries[i].key < entries[j].key
		}
		return entries[i].name < entries[j].name
	})
	for _, e := range entries {
		_, eds, _ := g.Instance(e.t, e.name)
		ds = append(ds, eds...)
	}

	paths := make([]string, 0, len(g.lazyByPath))
	for p := range g.lazyByPath {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		_, pds, _ := g.InstancePath(p)
		ds = append(ds, pds...)
	}
	for _, fn := range g.epilogues {
		ds = append(ds, fn()...)
	}
	return ds
}

func (g *Gen) writeScopeFuncs(b *bytes.Buffer) {
	ctxPkg := g.imports["context"]
	for _, s := range g.scopes {
		if len(s.nodes) == 0 {
			continue
		}
		results := strings.Join(s.resultTypes, ", ")
		if len(s.resultTypes) > 0 {
			results += ", "
		}
		b.WriteString("func " + s.fn + "(" + s.ctxVar + " " + ctxPkg + ".Context) (" + results + "error) {\n")
		g.emitScopedBody(b, s)
		b.WriteString("}\n\n")
	}
}

func (g *Gen) emitScopedBody(b *bytes.Buffer, s *Scope) {
	zeros := strings.Join(s.zeros, ", ")
	if zeros != "" {
		zeros += ", "
	}
	errsPkg := ""
	if s.hasCleanup {
		errsPkg = g.Import("errors")
	}

	if nodesHaveCheck(s.nodes) {
		b.WriteString("var err error\n")
	}

	var accumulated []string
	hasCleanup := false
	first := true
	for _, pl := range phaseLabels {
		var nodes []phaseNode
		for i, n := range s.nodes {
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
				ec := &errCtx{zeros: zeros, accumulated: accumulated, errsPkg: errsPkg}
				for _, line := range renderNode(pn.n, ec) {
					b.WriteString(line)
					b.WriteString("\n")
				}
				if c, ok := pn.n.(*Call); ok && c.Cleanup != "" {
					accumulated = append(accumulated, c.Cleanup)
					hasCleanup = true
				}
			}
		}
	}

	rets := strings.Join(s.rootExprs, ", ")
	if hasCleanup {
		b.WriteString("\ncleanup := func() error {\n")
		b.WriteString("var errs []error\n")
		for i := len(accumulated) - 1; i >= 0; i-- {
			b.WriteString("if " + accumulated[i] + " != nil {\n")
			b.WriteString("errs = append(errs, " + accumulated[i] + "())\n")
			b.WriteString("}\n")
		}
		b.WriteString("return " + errsPkg + ".Join(errs...)\n")
		b.WriteString("}\n")
		if rets != "" {
			rets += ", "
		}
		rets += "cleanup"
	}
	if rets != "" {
		rets += ", "
	}
	b.WriteString("return " + rets + "nil\n")
}

// nodesHaveCheck reports whether any node, including Select children,
// renders a Raw err check and therefore needs err pre-declared.
func nodesHaveCheck(nodes []Node) bool {
	for _, n := range nodes {
		switch n := n.(type) {
		case *Raw:
			if n.Check {
				return true
			}
		case *Select:
			for _, c := range n.Cases {
				if nodesHaveCheck(c.Body) {
					return true
				}
			}
		}
	}
	return false
}

func unwindLines(accumulated []string, errsPkg string) []string {
	var lines []string
	for i := len(accumulated) - 1; i >= 0; i-- {
		lines = append(lines,
			"if "+accumulated[i]+" != nil {",
			"err = "+errsPkg+".Join(err, "+accumulated[i]+"())",
			"}")
	}
	return lines
}

func zeroExpr(g *Gen, t types.Type) string {
	switch u := types.Unalias(t).Underlying().(type) {
	case *types.Pointer, *types.Interface, *types.Map, *types.Slice, *types.Chan, *types.Signature:
		return "nil"
	case *types.Basic:
		switch {
		case u.Kind() == types.UnsafePointer:
			return "nil"
		case u.Info()&types.IsString != 0:
			return `""`
		case u.Info()&types.IsBoolean != 0:
			return "false"
		default:
			return "0"
		}
	default:
		return g.TypeExpr(t) + "{}"
	}
}
