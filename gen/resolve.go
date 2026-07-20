package gen

import (
	"go/types"
)

// IsContext reports whether t is context.Context.
func IsContext(t types.Type) bool {
	return types.TypeString(types.Unalias(t), nil) == "context.Context"
}

// Context marks the lifecycle context as needed and returns its
// variable; Render emits the assignment as run()'s first statement.
func (g *Gen) Context() string {
	if sc := g.scope; sc != nil {
		return sc.ctxVar
	}
	if !g.ctxNeeded {
		g.ctxNeeded = true
		g.ctxPkg = g.Import("context")
		g.ctxVar = g.takeIdent("ctx")
	}
	return g.ctxVar
}

// Hinter improves missing-binding diagnostics with domain-specific help.
type Hinter interface {
	MissingHint(t types.Type) (string, bool)
}

// AddMissingHint registers a missing-binding help source.
func (g *Gen) AddMissingHint(f func(types.Type) (string, bool)) {
	g.hints = append(g.hints, f)
}

// MissingHint returns the first domain help that applies to t.
func (g *Gen) MissingHint(t types.Type) (string, bool) {
	for _, f := range g.hints {
		if h, ok := f(t); ok {
			return h, true
		}
	}
	return "", false
}
