package core

import (
	"fmt"
	"go/token"
	"go/types"
	"sort"
	"strings"

	cfgdir "github.com/gofabrik/fabrik/config/directive"
	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

// Select is the //fabrik:provider:select directive: declared on an
// interface with a config key, it wires the implementation the
// configuration names. Implementations are providers with case=<kind>,
// matched to the interface by their return type - directives never name
// code.
type Select struct {
	providers *Provider
	cfg       *cfgdir.Config
}

// NewSelect returns the selection directive for one run.
func NewSelect(providers *Provider, cfg *cfgdir.Config) *Select {
	return &Select{providers: providers, cfg: cfg}
}

func (*Select) Name() string { return "provider:select" }

func (*Select) Meta() gen.Meta {
	return gen.Meta{
		Synopsis: "Config-selected implementation: <config-key>",
		Doc: "**`//fabrik:provider:select <config-key>`**\n\n" +
			"Declared on an interface: the configuration value under the key " +
			"decides which implementation is wired at startup. The key must " +
			"match a string field of a //fabrik:config struct; its `default:` " +
			"tag is the fallback and must name a known case. " +
			"Implementations are providers with " +
			"`case=<kind>`, matched by their return type implementing the " +
			"interface. An unmatched value aborts startup naming it.\n\n" +
			"```go\n//fabrik:provider:select store.kind\ntype Store interface { ... }\n```",
		Example: "//fabrik:provider:select store.kind",
		Pos: []gen.PosSpec{
			{Name: "CONFIG-KEY", Kind: gen.KindFreeform},
		},
	}
}

type selectNode struct {
	key string
	pos token.Position

	grp *selGroup
}

func (sel *Select) Parse(a gen.Annotation) (any, diag.Diagnostics) {
	args, ds := gen.ParseArgs(a, sel.Meta())
	if len(args.Pos) < 1 {
		return nil, ds
	}
	if ds.HasFatal() {
		return nil, ds
	}
	return &selectNode{key: args.Pos[0].Text, pos: a.Pos}, ds
}

func (sel *Select) Check(n any, t gen.Typed) diag.Diagnostics {
	nd := n.(*selectNode)
	var ds diag.Diagnostics

	tn, ok := t.Target.(*types.TypeName)
	if !ok {
		ds.Error(nd.pos, "//fabrik:provider:select must be on an interface type declaration", "")
		return ds
	}
	if _, ok := tn.Type().Underlying().(*types.Interface); !ok {
		ds.Error(nd.pos, fmt.Sprintf("//fabrik:provider:select must be on an interface type (%s is not an interface)", tn.Name()),
			"the selection chooses between implementations of an interface")
		return ds
	}
	if named, ok := types.Unalias(tn.Type()).(*types.Named); ok && named.TypeParams().Len() > 0 {
		ds.Error(nd.pos, fmt.Sprintf("//fabrik:provider:select cannot be on a generic interface (%s has type parameters)", tn.Name()),
			"declare a concrete interface")
		return ds
	}
	if key := types.TypeString(types.Unalias(tn.Type()), nil); hasKey(sel.providers.seen, key) {
		ds.Error(nd.pos, fmt.Sprintf("type %s already has a plain provider", key),
			"a type is either provided directly or selected between implementations, not both")
		return ds
	}
	grp := sel.providers.group(tn.Type())
	if grp.sel != nil {
		ds.Error(nd.pos, fmt.Sprintf("duplicate //fabrik:provider:select on %s", tn.Name()),
			fmt.Sprintf("first declared at %s", grp.sel.pos))
		return ds
	}
	nd.grp = grp
	grp.sel = nd
	return ds
}

// Emit registers the group's interface binding once.
func (sel *Select) Emit(n any, g *gen.Gen) diag.Diagnostics {
	nd := n.(*selectNode)
	if nd.grp != nil {
		sel.providers.registerGroup(nd.grp, g)
	}
	return nil
}

// selGroup is one selected interface with its implementations.
type selGroup struct {
	iface      types.Type
	sel        *selectNode
	registered bool
	built      bool
}

// group returns the selGroup for an interface type, creating it on demand.
func (p *Provider) group(iface types.Type) *selGroup {
	key := types.TypeString(types.Unalias(iface), nil)
	if p.groups == nil {
		p.groups = map[string]*selGroup{}
	}
	grp, ok := p.groups[key]
	if !ok {
		grp = &selGroup{iface: iface}
		p.groups[key] = grp
	}
	return grp
}

// groupImpls returns the case providers whose return type implements the
// group's interface, sorted by case value.
func (p *Provider) groupImpls(grp *selGroup) []*node {
	var impls []*node
	for _, nd := range p.caseNodes {
		if nd.returns != nil && types.AssignableTo(nd.returns[0], grp.iface) {
			impls = append(impls, nd)
		}
	}
	sort.Slice(impls, func(i, j int) bool { return impls[i].caseVal < impls[j].caseVal })
	return impls
}

// registerGroup lazily binds the group's interface once.
func (p *Provider) registerGroup(grp *selGroup, g *gen.Gen) {
	if grp.registered {
		return
	}
	grp.registered = true
	g.BindLazy(grp.iface, "", func() (string, diag.Diagnostics) {
		return p.buildGroup(grp, g)
	})
}

// buildGroup emits the typed config read and the implementation switch.
// Case arms load their own configuration, so only the selected
// implementation costs anything at startup.
func (p *Provider) buildGroup(grp *selGroup, g *gen.Gen) (string, diag.Diagnostics) {
	var ds diag.Diagnostics
	grp.built = true

	named := types.Unalias(grp.iface).(*types.Named)
	base := named.Obj().Pkg().Name() + named.Obj().Name()

	cfgNode, kf, vds := p.validateSelection(grp)
	ds = append(ds, vds...)
	if cfgNode == nil {
		return "nil", ds
	}
	cfgVar, eds, cok := g.Instance(types.NewPointer(cfgNode.Named()), "")
	ds = append(ds, anchor(eds, grp.sel.pos)...)
	if !cok {
		return "nil", ds
	}

	kindVar := g.Var(base + "Kind")
	g.Stmt(gen.PhaseWire, "%s := %s.%s", kindVar, cfgVar, strings.Join(kf.Path, "."))

	v := g.Var(base)
	g.Stmt(gen.PhaseWire, "var %s %s", v, g.TypeExpr(grp.iface))

	impls := p.groupImpls(grp)
	var b strings.Builder
	fmt.Fprintf(&b, "switch %s {\n", kindVar)
	emitted := map[string]bool{}
	for _, impl := range impls {
		if emitted[impl.caseVal] { // duplicate, diagnosed by validateSelection
			continue
		}
		emitted[impl.caseVal] = true
		impl.built = true
		fmt.Fprintf(&b, "case %q:\n", impl.caseVal)
		args, ids := p.resolveCaseParams(g, impl, cfgNode, cfgVar, &b)
		ds = append(ds, ids...)
		call := fmt.Sprintf("%s.%s(%s)", g.ImportPkg(impl.pkg), impl.fn, strings.Join(args, ", "))
		if impl.returnsErr {
			tmp := g.Var(base + exportish(impl.caseVal))
			fmt.Fprintf(&b, "%s, err := %s\nif err != nil {\nreturn err\n}\n%s = %s\n", tmp, call, v, tmp)
		} else {
			fmt.Fprintf(&b, "%s = %s\n", v, call)
		}
	}
	fmt.Fprintf(&b, "default:\nreturn %s.Errorf(\"no %s implementation for %%q\", %s)\n}",
		g.Import("fmt"), g.TypeExpr(grp.iface), kindVar)
	g.Stmt(gen.PhaseWire, "%s", b.String())
	return v, ds
}

// validateSelection checks a group's static contract - the key resolves
// to a string-kinded field, case values are unique, and the default
// names a known case - so emission and finish share one invariant. A nil
// cfgNode means the key is unusable.
func (p *Provider) validateSelection(grp *selGroup) (*cfgdir.Node, cfgdir.Key, diag.Diagnostics) {
	var ds diag.Diagnostics
	cfgNode, kf, ok := p.cfg.ResolveKey(grp.sel.key)
	if !ok {
		ds.Error(grp.sel.pos, fmt.Sprintf("config key %q does not match any //fabrik:config field", grp.sel.key),
			"declare the key as a string field of a //fabrik:config struct")
		return nil, cfgdir.Key{}, ds
	}
	if !isStringKind(kf.Type) {
		ds.Error(grp.sel.pos, fmt.Sprintf("config key %q is %s, not string", grp.sel.key, types.TypeString(kf.Type, nil)),
			"the selection switches on a string kind")
		return nil, cfgdir.Key{}, ds
	}
	impls := p.groupImpls(grp)
	seen := map[string]token.Position{}
	for _, impl := range impls {
		if first, dup := seen[impl.caseVal]; dup {
			ds.Error(impl.pos, fmt.Sprintf("duplicate implementation for case=%s", impl.caseVal),
				fmt.Sprintf("first declared at %s", first))
		} else {
			seen[impl.caseVal] = impl.pos
		}
	}
	ds = append(ds, checkDefaultCase(grp, kf, impls)...)
	return cfgNode, kf, ds
}

// isStringKind reports whether t's underlying type is string, including
// defined kinds like `type Kind string` - the switch compares against
// untyped string constants, which convert either way.
func isStringKind(t types.Type) bool {
	b, ok := types.Unalias(t).Underlying().(*types.Basic)
	return ok && b.Info()&types.IsString != 0
}

// checkDefaultCase rejects a default: tag naming no case= implementation:
// the fallback would fail at startup, and both sides are static source.
func checkDefaultCase(grp *selGroup, kf cfgdir.Key, impls []*node) diag.Diagnostics {
	var ds diag.Diagnostics
	if kf.Default == "" {
		return ds
	}
	var known []string
	for _, impl := range impls {
		if impl.caseVal == kf.Default {
			return ds
		}
		if len(known) == 0 || known[len(known)-1] != impl.caseVal {
			known = append(known, impl.caseVal)
		}
	}
	ds.Error(grp.sel.pos, fmt.Sprintf("config key %q defaults to %q, which matches no case= implementation", grp.sel.key, kf.Default),
		fmt.Sprintf("known cases: %s; add a provider with case=%s or fix the default: tag", strings.Join(known, ", "), kf.Default))
	return ds
}

// resolveCaseParams resolves a case provider's parameters: configuration
// and context only. A config struct other than the selector's own loads
// inside the case arm, so unselected implementations cost nothing at
// startup. Anything heavier belongs inside the constructor.
func (p *Provider) resolveCaseParams(g *gen.Gen, nd *node, keyNode *cfgdir.Node, keyVar string, body *strings.Builder) ([]string, diag.Diagnostics) {
	return resolveArgs(g, p.cfg, nd.params,
		func(pr param) (string, diag.Diagnostics, bool) {
			cn := p.cfg.NodeFor(pr.t)
			if cn == nil {
				return "", nil, false
			}
			if cn == keyNode {
				return keyVar, nil, true
			}
			v, stmt := p.cfg.LoadStmt(cn, g)
			fmt.Fprintf(body, "%s\n", stmt)
			return v, nil, true
		},
		func(param) (string, string) {
			return "case provider parameters must be //fabrik:config structs or context.Context",
				"construct other dependencies inside the provider, so unselected implementations cost nothing"
		})
}

// finishGroups validates selection completeness after emission.
func (p *Provider) finishGroups(ds *diag.Diagnostics) {
	keys := make([]string, 0, len(p.groups))
	for key := range p.groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		grp := p.groups[key]
		if grp.sel == nil {
			continue
		}
		impls := p.groupImpls(grp)
		if len(impls) == 0 {
			ds.Error(grp.sel.pos, "no implementations for this selection",
				"add providers with case=<kind> whose return type implements the interface")
			continue
		}
		if !grp.built {
			_, _, vds := p.validateSelection(grp)
			*ds = append(*ds, vds...)
			for _, impl := range impls {
				for _, pr := range impl.params {
					if gen.IsContext(pr.t) || p.cfg.IsConfig(pr.t) {
						continue
					}
					ds.Error(pr.pos, "case provider parameters must be //fabrik:config structs or context.Context",
						missingHelp(p.cfg, pr.t, "construct other dependencies inside the provider, so unselected implementations cost nothing"))
				}
			}
		}
	}
	// Case providers must belong to exactly one selected interface.
	for _, nd := range p.caseNodes {
		if nd.returns == nil {
			continue
		}
		var matches []string
		for _, grp := range p.groups {
			if types.AssignableTo(nd.returns[0], grp.iface) {
				matches = append(matches, types.TypeString(types.Unalias(grp.iface), nil))
			}
		}
		sort.Strings(matches)
		switch len(matches) {
		case 1:
		case 0:
			ds.Error(nd.pos, "case= provider matches no //fabrik:provider:select interface",
				"declare //fabrik:provider:select <config-key> on the interface this type implements")
		default:
			ds.Error(nd.pos, fmt.Sprintf("case= provider matches multiple selected interfaces: %s", strings.Join(matches, ", ")),
				"a case provider must implement exactly one selected interface")
		}
	}
}

func hasKey(m map[string]token.Position, key string) bool {
	_, ok := m[key]
	return ok
}

// exportish renders a case value as an identifier suffix.
func exportish(s string) string {
	var b strings.Builder
	up := true
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			if up {
				b.WriteString(strings.ToUpper(string(r)))
				up = false
			} else {
				b.WriteRune(r)
			}
			continue
		}
		up = true
	}
	return b.String()
}
