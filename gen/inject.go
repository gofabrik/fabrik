package gen

import "go/types"

// injectKey addresses one mapping: the annotated declaration and the
// parameter or field selector on it.
type injectKey struct {
	obj      types.Object
	selector string
}

type injectState int

const (
	injectPending injectState = iota
	injectConsumed
	injectRejected
)

// SeedInjectNames installs the inject mapping table. Seeding happens
// before any Emit runs, so lookups are order-independent within a tier.
func (g *Gen) SeedInjectNames(table map[types.Object]map[string]string) {
	if g.inject == nil {
		g.inject = map[injectKey]*injectEntry{}
	}
	for obj, sels := range table {
		for sel, name := range sels {
			g.inject[injectKey{obj, sel}] = &injectEntry{name: name}
		}
	}
}

type injectEntry struct {
	name  string
	state injectState
}

// InjectName reports the provider name mapped to a declaration's
// parameter or field. It is a pure lookup; callers that act on the
// mapping record that with ConsumeInject or RejectInject.
func (g *Gen) InjectName(obj types.Object, selector string) (string, bool) {
	e, ok := g.inject[injectKey{obj, selector}]
	if !ok {
		return "", false
	}
	return e.name, true
}

// ConsumeInject marks a mapping as used by generated wiring.
func (g *Gen) ConsumeInject(obj types.Object, selector string) {
	if e, ok := g.inject[injectKey{obj, selector}]; ok {
		e.state = injectConsumed
	}
}

// RejectInject marks a mapping a consumer refused with its own
// diagnostic, so the generic no-effect check does not report it again.
func (g *Gen) RejectInject(obj types.Object, selector string) {
	if e, ok := g.inject[injectKey{obj, selector}]; ok {
		e.state = injectRejected
	}
}

// InjectPending reports whether a mapping was neither consumed nor
// rejected.
func (g *Gen) InjectPending(obj types.Object, selector string) bool {
	e, ok := g.inject[injectKey{obj, selector}]
	return ok && e.state == injectPending
}

// BindingOwner reports which directive registered the lazy binding for
// (t, name).
func (g *Gen) BindingOwner(t types.Type, name string) (string, bool) {
	t = types.Unalias(t)
	m, _ := g.lazy.At(t).(map[string]*lazyBind)
	if b, ok := m[name]; ok {
		return b.owner, true
	}
	return "", false
}
