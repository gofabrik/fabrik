package gen

import (
	"go/types"
)

// IsContext reports whether t is context.Context.
func IsContext(t types.Type) bool {
	return types.TypeString(types.Unalias(t), nil) == "context.Context"
}

// Context returns the shared background context singleton.
func (g *Gen) Context() string {
	return g.SingletonIn(PhaseInit, "context", "ctx", g.Import("context")+".Background()")
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
