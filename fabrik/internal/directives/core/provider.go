// Package core implements CLI-owned directives.
package core

import (
	"fmt"
	"go/token"
	"go/types"
	"strings"

	cfgdir "github.com/gofabrik/fabrik/config/directive"
	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

// Provider is the //fabrik:provider directive.
type Provider struct {
	seen      map[string]token.Position
	nodes     []*node
	caseNodes []*node
	groups    map[string]*selGroup
	cfg       *cfgdir.Config
}

// NewProvider returns a Provider directive for one run, sharing the config
// directive for switch-key reads.
func NewProvider(cfg *cfgdir.Config) *Provider {
	return &Provider{seen: map[string]token.Position{}, cfg: cfg}
}

func (*Provider) Name() string { return "provider" }

func (*Provider) Meta() gen.Meta {
	return gen.Meta{
		Synopsis: "Constructor wired by return type",
		Doc: "**`//fabrik:provider [case=kind]`**\n\n" +
			"Marks a constructor whose return value is available to generated " +
			"app code by matching types. Parameters resolve to other " +
			"providers; `context.Context` parameters receive a shared " +
			"background context. A second `error` return aborts startup. " +
			"With `case=<kind>`, the constructor is instead one selectable " +
			"implementation for a `//fabrik:provider:select` interface, " +
			"matched by its return type and constructed only when the " +
			"configuration names its kind.\n\n" +
			"```go\n//fabrik:provider\nfunc NewGreeter() *Greeter { ... }\n```",
		Example: "//fabrik:provider",
		Attrs: []gen.AttrSpec{
			{Key: "case", Kind: gen.KindFreeform},
		},
	}
}

type param struct {
	t   types.Type
	pos token.Position
}

type node struct {
	pos token.Position

	caseVal string // case=kind: one selected implementation, matched by type

	fn         string
	pkg        *types.Package
	returns    []types.Type
	returnsErr bool
	params     []param
	fset       *token.FileSet
	iface      types.Type
	grp        *selGroup
	built      bool // lazy build ran
}

func (p *Provider) Parse(a gen.Annotation) (any, diag.Diagnostics) {
	args, ds := gen.ParseArgs(a, p.Meta())
	nd := &node{pos: a.Pos}
	if caseA, hasCase := args.Attr["case"]; hasCase {
		nd.caseVal = caseA.Text
		if nd.caseVal == "" {
			ds.Error(a.ArgPos(caseA.Col), "case= must not be empty",
				"example: //fabrik:provider case=memory")
		}
	}
	if ds.HasFatal() {
		return nil, ds
	}
	return nd, ds
}

func (p *Provider) Check(n any, t gen.Typed) diag.Diagnostics {
	nd := n.(*node)
	var ds diag.Diagnostics

	fn, ok := t.Target.(*types.Func)
	if !ok {
		ds.Error(nd.pos, "//fabrik:provider must be on a function", "")
		return ds
	}
	sig := fn.Signature()
	if sig.Recv() != nil {
		ds.Error(nd.pos, fmt.Sprintf("//fabrik:provider must be on a package-level function (func %s is a method)", fn.Name()),
			"move the constructor out of the method set")
		return ds
	}
	if sig.TypeParams().Len() > 0 {
		ds.Error(nd.pos, fmt.Sprintf("provider %s cannot be generic (generated code cannot infer type arguments)", fn.Name()),
			"declare a concrete constructor")
		return ds
	}

	results := sig.Results()
	switch {
	case results.Len() == 0:
		ds.Error(nd.pos, fmt.Sprintf("//fabrik:provider requires a return value (func %s returns nothing)", fn.Name()),
			"example: func NewGreeter() *Greeter")
		return ds
	case results.Len() == 2 && isErrorType(results.At(1).Type()):
		nd.returnsErr = true
	case results.Len() > 1:
		ds.Error(nd.pos, fmt.Sprintf("provider %s must return exactly one value, or a value and an error", fn.Name()),
			"example: func NewGreeter() (*Greeter, error)")
		return ds
	}

	nd.fn = fn.Name()
	nd.pkg = fn.Pkg()
	nd.returns = []types.Type{types.Unalias(results.At(0).Type())}
	nd.fset = t.Fset
	for i := 0; i < sig.Params().Len(); i++ {
		v := sig.Params().At(i)
		nd.params = append(nd.params, param{t: v.Type(), pos: t.Fset.Position(v.Pos())})
	}

	if nd.caseVal != "" {
		// Case providers match a selected interface by type; membership is
		// validated once every selection is known.
		p.caseNodes = append(p.caseNodes, nd)
		return ds
	}

	key := types.TypeString(nd.returns[0], nil)
	if first, dup := p.seen[key]; dup {
		ds.Error(nd.pos, fmt.Sprintf("multiple providers for type %s", key),
			fmt.Sprintf("only one //fabrik:provider per type is supported; first declared at %s", first))
		return ds
	}
	if _, grouped := p.groups[key]; grouped {
		ds.Error(nd.pos, fmt.Sprintf("type %s is provided by conditional implementations", key),
			"a type is either provided directly or selected between implementations, not both")
		return ds
	}
	p.seen[key] = nd.pos
	return ds
}

// Emit registers the provider lazily.
func (p *Provider) Emit(n any, g *gen.Gen) diag.Diagnostics {
	nd := n.(*node)
	p.nodes = append(p.nodes, nd)
	if nd.caseVal != "" {
		// Bound through the selected interface's group, registered by
		// //fabrik:provider:select.
		return nil
	}
	g.BindLazy(nd.returns[0], "", func() (string, diag.Diagnostics) {
		nd.built = true
		args, ds := p.resolveParams(g, nd.params)
		v := g.Var(varBase(nd.pkg, nd.returns[0]))
		if nd.returnsErr {
			g.Stmt(gen.PhaseWire, "%s, err := %s.%s(%s)\nif err != nil {\nreturn err\n}", v, g.ImportPkg(nd.pkg), nd.fn, strings.Join(args, ", "))
		} else {
			g.Stmt(gen.PhaseWire, "%s := %s.%s(%s)", v, g.ImportPkg(nd.pkg), nd.fn, strings.Join(args, ", "))
		}
		return v, ds
	})
	return nil
}

// Finish validates group completeness and unused provider parameters.
func (p *Provider) Finish(g *gen.Gen) diag.Diagnostics {
	var ds diag.Diagnostics
	p.finishGroups(&ds)
	for _, nd := range p.nodes {
		if nd.built || nd.caseVal != "" {
			continue
		}
		for _, pr := range nd.params {
			if types.TypeString(types.Unalias(pr.t), nil) == "context.Context" {
				continue
			}
			if !g.HasBinding(pr.t, "") {
				ds.Error(pr.pos, fmt.Sprintf("no provider for %s", g.TypeExpr(pr.t)), p.paramHelp(g, pr.t))
			}
		}
	}
	return ds
}

// resolveParams builds call arguments for wired parameters.
func (p *Provider) resolveParams(g *gen.Gen, params []param) ([]string, diag.Diagnostics) {
	var ds diag.Diagnostics
	args := make([]string, 0, len(params))
	for _, pr := range params {
		if types.TypeString(types.Unalias(pr.t), nil) == "context.Context" {
			args = append(args, g.SingletonIn(gen.PhaseInit, "context", "ctx", g.Import("context")+".Background()"))
			continue
		}
		expr, eds, ok := g.Instance(pr.t, "")
		if !ok {
			if len(eds) == 0 {
				ds.Error(pr.pos, fmt.Sprintf("no provider for %s", g.TypeExpr(pr.t)), p.paramHelp(g, pr.t))
			}
			expr = "nil"
		}
		ds = append(ds, anchor(eds, pr.pos)...)
		args = append(args, expr)
	}
	return args, ds
}

// paramHelp suggests the fix for an unresolvable parameter.
func (p *Provider) paramHelp(g *gen.Gen, t types.Type) string {
	if p.cfg.IsConfigValue(t) {
		return fmt.Sprintf("config structs are injected as pointers; take *%s", g.TypeExpr(t))
	}
	return fmt.Sprintf("add a //fabrik:provider returning %s", g.TypeExpr(t))
}

// anchor fills missing diagnostic positions.
func anchor(ds diag.Diagnostics, pos token.Position) diag.Diagnostics {
	for i := range ds {
		if !ds[i].Pos.IsValid() {
			ds[i].Pos = pos
		}
	}
	return ds
}

// varBase derives the generated variable name for a provided value.
func varBase(pkg *types.Package, t types.Type) string {
	t = types.Unalias(t)
	if ptr, ok := t.(*types.Pointer); ok {
		t = types.Unalias(ptr.Elem())
	}
	if named, ok := t.(*types.Named); ok {
		return pkg.Name() + named.Obj().Name()
	}
	return pkg.Name() + "Value"
}

func isErrorType(t types.Type) bool {
	return types.TypeString(types.Unalias(t), nil) == "error"
}
