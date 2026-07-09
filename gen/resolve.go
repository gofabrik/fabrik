package gen

import (
	"go/types"
)

// IsContext reports whether t is context.Context.
func IsContext(t types.Type) bool {
	return types.TypeString(types.Unalias(t), nil) == "context.Context"
}

// Context returns the shared background context singleton, emitted in the
// Init phase on first use. Every directive that accepts context.Context
// parameters resolves them here, so the construction exists exactly once.
func (g *Gen) Context() string {
	return g.SingletonIn(PhaseInit, "context", "ctx", g.Import("context")+".Background()")
}

// Hinter is implemented by directives that can improve a missing-binding
// diagnostic with domain knowledge (e.g. "take *Config" when a config
// struct is referenced by value). The engine installs all hinters on the
// Gen before emission.
type Hinter interface {
	MissingHint(t types.Type) (string, bool)
}

// AddMissingHint registers a domain help source for missing-binding
// diagnostics.
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
