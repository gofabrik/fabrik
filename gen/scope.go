package gen

import (
	"go/token"
	"go/types"
	"slices"
	"sort"

	"github.com/gofabrik/fabrik/diag"
)

// ScopeRoot selects a command dependency by type and provider name.
type ScopeRoot struct {
	Type types.Type
	Name string
}

// Scope contains the generated state for one command build function.
type Scope struct {
	fn    string
	pos   token.Position
	roots []ScopeRoot

	idents      map[string]bool
	reserved    map[string]bool
	binds       map[string]map[string]string // printed type -> name -> expr
	bindOrigins map[string]map[string]string // printed type -> name -> owner
	bindTypes   map[string]types.Type
	bindOrder   []string
	bindSeen    map[string]bool
	pathExprs   map[string]string
	singletons  map[string]string
	onceVals    map[string]string
	nodes       []Node
	running     map[*lazyBind]bool
	imports     map[string]string // isolated validation aliases
	ctxVar      string
	validation  bool

	rootExprs []string
}

// AddScope registers a build scope for the dependency roots at pos.
func (g *Gen) AddScope(fn string, pos token.Position, roots ...ScopeRoot) *Scope {
	if len(g.scopes) == 0 {
		// Reserve wrapper identifiers before assigning import aliases.
		g.Context()
		g.idents["cleanup"] = true
		g.idents["err"] = true
		g.idents["unwind"] = true
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
	s.idents["unwind"] = true
	s.ctxVar = "ctx"
	s.idents["ctx"] = true
	// Late alias names constrain generated locals but remain available to imports.
	s.reserved = map[string]bool{}
	for _, a := range lateAliases {
		s.reserved[a] = true
	}
	s.binds = map[string]map[string]string{}
	s.bindOrigins = map[string]map[string]string{}
	s.bindTypes = map[string]types.Type{}
	s.bindOrder = nil
	s.bindSeen = map[string]bool{}
	s.pathExprs = map[string]string{}
	s.singletons = map[string]string{}
	s.onceVals = map[string]string{}
	s.running = map[*lazyBind]bool{}
	s.validation = validation
	if validation {
		s.imports = map[string]string{}
	}
	g.scope = s
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
		if g.demanded(demandKey{kind: demandType, key: e.key, name: e.name}) {
			// Flow materialization owns this diagnostic.
			continue
		}
		_, eds, _ := g.Instance(e.t, e.name)
		ds = append(ds, eds...)
	}

	paths := make([]string, 0, len(g.lazyByPath))
	for p := range g.lazyByPath {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if g.demanded(demandKey{kind: demandPath, key: p}) {
			continue
		}
		_, pds, _ := g.InstancePath(p)
		ds = append(ds, pds...)
	}
	for i, fn := range g.epilogues {
		ds = append(ds, g.reportCallbackDiags(i, fn())...)
	}
	return ds
}

// lateAliases must include every import Render can register after scope identifiers freeze.
var lateAliases = []string{"os", "fmt", "errors", "signal", "syscall", "cli", "context"}

// nodeHasErrTail determines whether a cleanup-bearing scope needs its unwind helper.
func nodeHasErrTail(n Node) bool {
	switch n := n.(type) {
	case *Call:
		return n.Err != ErrNone
	case *ConfigLoad, *Select:
		return true
	case *Raw:
		return n.Check
	}
	return false
}

// nodesHaveCheck determines whether err must be declared before Raw checks.
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
	for _, a := range slices.Backward(accumulated) {
		lines = append(lines,
			"if "+a+" != nil {",
			"err = "+errsPkg+".Join(err, "+a+"())",
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
