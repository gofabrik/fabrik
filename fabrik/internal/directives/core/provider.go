// Package core implements framework-owned directives.
package core

import (
	"fmt"
	"go/token"
	"go/types"
	"strings"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

// Provider is the //fabrik:provider directive.
type Provider struct {
	seen  map[string]token.Position
	nodes []*node
}

// NewProvider returns a Provider directive for one run.
func NewProvider() *Provider { return &Provider{seen: map[string]token.Position{}} }

func (*Provider) Name() string { return "provider" }

func (*Provider) Meta() gen.Meta {
	return gen.Meta{
		Synopsis: "Constructor wired by return type",
		Doc: "**`//fabrik:provider`**\n\n" +
			"Marks a constructor whose return value is injected into handler " +
			"structs and other providers by matching types. Parameters resolve " +
			"to other providers; `context.Context` parameters receive a shared " +
			"background context.\n\n" +
			"```go\n//fabrik:provider\nfunc NewGreeter() *Greeter { ... }\n```",
		Example: "//fabrik:provider",
	}
}

type param struct {
	t   types.Type
	pos token.Position
}

type node struct {
	pos token.Position

	fn      string
	pkg     *types.Package
	returns []types.Type
	params  []param
	fset    *token.FileSet
	built   bool // the lazy build ran, so params were already validated
}

func (p *Provider) Parse(a gen.Annotation) (any, diag.Diagnostics) {
	_, ds := gen.ParseArgs(a, p.Meta())
	if ds.HasFatal() {
		return nil, ds
	}
	return &node{pos: a.Pos}, ds
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

	results := sig.Results()
	switch {
	case results.Len() == 0:
		ds.Error(nd.pos, fmt.Sprintf("//fabrik:provider requires a return value (func %s returns nothing)", fn.Name()),
			"example: func NewGreeter() *Greeter")
		return ds
	case results.Len() == 2 && isErrorType(results.At(1).Type()):
		ds.Error(nd.pos, fmt.Sprintf("provider %s returns (%s, error), which is not supported yet", fn.Name(), types.TypeString(results.At(0).Type(), types.RelativeTo(fn.Pkg()))),
			"return the value only, or handle the error inside the provider")
		return ds
	case results.Len() > 1:
		ds.Error(nd.pos, fmt.Sprintf("provider %s must return exactly one value", fn.Name()),
			"example: func NewGreeter() *Greeter")
		return ds
	}

	ret := types.Unalias(results.At(0).Type())
	key := types.TypeString(ret, nil)
	if first, dup := p.seen[key]; dup {
		ds.Error(nd.pos, fmt.Sprintf("multiple providers for type %s", key),
			fmt.Sprintf("only one //fabrik:provider per type is supported; first declared at %s", first))
		return ds
	}
	p.seen[key] = nd.pos

	nd.fn = fn.Name()
	nd.pkg = fn.Pkg()
	nd.returns = []types.Type{ret}
	nd.fset = t.Fset
	for i := 0; i < sig.Params().Len(); i++ {
		v := sig.Params().At(i)
		nd.params = append(nd.params, param{t: v.Type(), pos: t.Fset.Position(v.Pos())})
	}
	return ds
}

// Emit registers the provider lazily.
func (p *Provider) Emit(n any, g *gen.Gen) diag.Diagnostics {
	nd := n.(*node)
	p.nodes = append(p.nodes, nd)
	g.BindLazy(nd.returns[0], "", func() (string, diag.Diagnostics) {
		nd.built = true
		args, ds := resolveParams(g, nd.params)
		v := g.Var(varBase(nd.pkg, nd.returns[0]))
		g.Stmt(gen.PhaseWire, "%s := %s.%s(%s)", v, g.ImportPkg(nd.pkg), nd.fn, strings.Join(args, ", "))
		return v, ds
	})
	return nil
}

// Finish validates the parameters of providers nothing materialized: their
// build closures never ran, so unresolvable dependencies would otherwise go
// undiagnosed until the provider is first consumed.
func (p *Provider) Finish(g *gen.Gen) diag.Diagnostics {
	var ds diag.Diagnostics
	for _, nd := range p.nodes {
		if nd.built {
			continue
		}
		for _, pr := range nd.params {
			if types.TypeString(types.Unalias(pr.t), nil) == "context.Context" {
				continue
			}
			if !g.HasBinding(pr.t, "") {
				ds.Error(pr.pos, fmt.Sprintf("no provider for %s", g.TypeExpr(pr.t)),
					fmt.Sprintf("add a //fabrik:provider returning %s", g.TypeExpr(pr.t)))
			}
		}
	}
	return ds
}

// resolveParams builds call arguments for wired parameters.
func resolveParams(g *gen.Gen, params []param) ([]string, diag.Diagnostics) {
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
				ds.Error(pr.pos, fmt.Sprintf("no provider for %s", g.TypeExpr(pr.t)),
					fmt.Sprintf("add a //fabrik:provider returning %s", g.TypeExpr(pr.t)))
			}
			expr = "nil"
		}
		ds = append(ds, anchor(eds, pr.pos)...)
		args = append(args, expr)
	}
	return args, ds
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
