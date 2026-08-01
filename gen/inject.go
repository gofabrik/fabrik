package gen

import (
	"fmt"
	"go/types"
	"sort"
	"strings"
)

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

// SeedInjectNames installs mappings before directive emission.
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

// InjectName returns a selector's provider name without consuming the mapping.
func (g *Gen) InjectName(obj types.Object, selector string) (string, bool) {
	e, ok := g.inject[injectKey{obj, selector}]
	if !ok {
		return "", false
	}
	return e.name, true
}

// SelectedName returns a selector's mapped provider name and marks the mapping consumed.
func (g *Gen) SelectedName(obj types.Object, selector string) string {
	name, ok := g.InjectName(obj, selector)
	if !ok {
		return ""
	}
	g.ConsumeInject(obj, selector)
	return name
}

// ConsumeInject marks a mapping as used by generated wiring.
func (g *Gen) ConsumeInject(obj types.Object, selector string) {
	if e, ok := g.inject[injectKey{obj, selector}]; ok {
		e.state = injectConsumed
	}
}

// RejectInject marks a mapping handled by a consumer diagnostic.
func (g *Gen) RejectInject(obj types.Object, selector string) {
	if e, ok := g.inject[injectKey{obj, selector}]; ok {
		e.state = injectRejected
	}
}

// InjectPending reports whether a mapping is neither consumed nor rejected.
func (g *Gen) InjectPending(obj types.Object, selector string) bool {
	e, ok := g.inject[injectKey{obj, selector}]
	return ok && e.state == injectPending
}

// BindingNames returns sorted provider names for t, including "" for unnamed.
func (g *Gen) BindingNames(t types.Type) []string {
	t = types.Unalias(t)
	key := types.TypeString(t, nil)
	set := map[string]bool{}
	// Match Instance visibility, including path bindings as unnamed.
	if sc := g.scope; sc != nil {
		for name := range sc.binds[key] {
			set[name] = true
		}
		if _, ok := sc.pathExprs[key]; ok {
			set[""] = true
		}
	} else {
		if m, ok := g.binds.At(t).(map[string]string); ok {
			for name := range m {
				set[name] = true
			}
		}
		if _, ok := g.pathExprs[key]; ok {
			set[""] = true
		}
	}
	if _, ok := g.lazyByPath[key]; ok {
		set[""] = true
	}
	if m, ok := g.lazy.At(t).(map[string]*lazyBind); ok {
		for name := range m {
			set[name] = true
		}
	}
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// MissingBinding returns a naming diagnostic, or fallback when no named provider is involved.
func (g *Gen) MissingBinding(t types.Type, name string, fallback func() (msg, help string)) (msg, help string) {
	names := g.BindingNames(t)
	if name == "" {
		// An unnamed binding makes the failure unrelated to provider naming.
		if len(names) == 0 || names[0] == "" {
			return fallback()
		}
		return fmt.Sprintf("type %s has only named providers", g.TypeExpr(t)),
			knownNames(names) + "; select one with //fabrik:inject"
	}
	msg = fmt.Sprintf("no provider for %s named %q", g.TypeExpr(t), name)
	if len(names) == 0 {
		return msg, fmt.Sprintf("add a //fabrik:provider name=%s returning %s", name, g.TypeExpr(t))
	}
	return msg, knownNames(names)
}

func knownNames(names []string) string {
	shown := make([]string, len(names))
	for i, n := range names {
		if n == "" {
			n = "(unnamed)"
		}
		shown[i] = n
	}
	return "known names: " + strings.Join(shown, ", ")
}

// BindingOwner reports which directive registered the lazy binding for (t, name).
func (g *Gen) BindingOwner(t types.Type, name string) (string, bool) {
	t = types.Unalias(t)
	m, _ := g.lazy.At(t).(map[string]*lazyBind)
	if b, ok := m[name]; ok {
		return b.owner, true
	}
	return "", false
}
