package directive

import (
	"fmt"
	"go/token"
	"go/types"
	"sort"
	"strings"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

// Middleware registers global and named HTTP middleware.
type Middleware struct {
	byName map[string]*mwNode
}

// NewMiddleware returns a Middleware directive for one run.
func NewMiddleware() *Middleware { return &Middleware{byName: map[string]*mwNode{}} }

func (*Middleware) Name() string { return "http:middleware" }

func (*Middleware) Meta() gen.Meta {
	return gen.Meta{
		Synopsis: "Middleware: direct or constructor form, global or named [name=NAME]",
		Doc: "**`//fabrik:http:middleware [name=NAME]`**\n\n" +
			"Direct form: `func(next http.Handler) http.Handler`, referenced in place. " +
			"Constructor form: binding-resolved parameters returning " +
			"`func(http.Handler) http.Handler` or `router.Middleware`, optionally with " +
			"a trailing error; it is built once before route registration. Bare " +
			"middleware is global, including 404/405. With `name=`, routes and groups " +
			"opt in through their `middleware=` chain.\n\n" +
			"```go\n//fabrik:http:middleware name=auth\nfunc RequireAuth(next http.Handler) http.Handler { ... }\n\n//fabrik:http:middleware\nfunc SessionMiddleware(m *session.Manager[Session]) func(http.Handler) http.Handler {\n\treturn m.Middleware\n}\n```",
		Example: "//fabrik:http:middleware",
		Attrs: []gen.AttrSpec{
			{Key: "name", Kind: gen.KindFreeform},
		},
	}
}

type mwNode struct {
	pos  token.Position
	name string // "" is global

	fn   string
	pkg  *types.Package
	used bool // referenced by at least one middleware= chain

	ctor      bool // constructor form: built once, referenced by variable
	errResult bool
	params    []ctorParam
	varName   string // set once the constructor is materialized
}

// ctorParam is one binding-resolved constructor parameter.
type ctorParam struct {
	t   types.Type
	pos token.Position
}

func (m *Middleware) Parse(a gen.Annotation) (any, diag.Diagnostics) {
	args, ds := gen.ParseArgs(a, m.Meta())
	nd := &mwNode{pos: a.Pos}
	if nm, ok := args.Attr["name"]; ok {
		nd.name = nm.Text
		if !isIdentifier(nd.name) {
			ds.Error(a.ArgPos(nm.Col), fmt.Sprintf("invalid middleware name %q", nd.name),
				"use a short identifier: name=auth")
		}
	}
	if ds.HasFatal() {
		return nil, ds
	}
	return nd, ds
}

func (m *Middleware) Check(n any, t gen.Typed) diag.Diagnostics {
	nd := n.(*mwNode)
	var ds diag.Diagnostics

	fn, ok := t.Target.(*types.Func)
	if !ok {
		ds.Error(nd.pos, "//fabrik:http:middleware must be on a function", "")
		return ds
	}
	sig := fn.Signature()
	if sig.Recv() != nil {
		ds.Error(nd.pos, fmt.Sprintf("//fabrik:http:middleware must be on a package-level function (func %s is a method)", fn.Name()),
			"move the middleware out of the method set")
		return ds
	}
	if isGenericFunc(fn) {
		ds.Error(nd.pos, fmt.Sprintf("middleware %s cannot be generic (generated code cannot instantiate type parameters)", fn.Name()),
			"declare a concrete middleware")
		return ds
	}
	switch {
	case isMiddlewareSignature(sig):
	case isCtorSignature(sig):
		nd.ctor = true
		nd.errResult = sig.Results().Len() == 2
		for j := 0; j < sig.Params().Len(); j++ {
			v := sig.Params().At(j)
			if types.TypeString(types.Unalias(v.Type()), nil) == "net/http.Handler" {
				ds.Error(nd.pos, fmt.Sprintf("middleware %s is neither form: not a direct middleware (it does not return http.Handler itself), not a constructor (its http.Handler parameter cannot resolve from the binding surface)", fn.Name()),
					"direct: func(next http.Handler) http.Handler; constructor: binding-resolved parameters returning the middleware")
				return ds
			}
			nd.params = append(nd.params, ctorParam{t: v.Type(), pos: t.Fset.Position(v.Pos())})
		}
	default:
		ds.Error(nd.pos, fmt.Sprintf("middleware %s has the wrong signature", fn.Name()),
			"want func(next http.Handler) http.Handler, or a constructor returning func(http.Handler) http.Handler or router.Middleware (optionally with error)")
		return ds
	}
	if nd.name != "" {
		if first, dup := m.byName[nd.name]; dup {
			ds.Error(nd.pos, fmt.Sprintf("duplicate middleware name %q", nd.name),
				fmt.Sprintf("first declared at %s", first.pos))
			return ds
		}
		m.byName[nd.name] = nd
	}

	nd.fn = fn.Name()
	nd.pkg = fn.Pkg()
	return ds
}

func (m *Middleware) Emit(n any, g *gen.Gen) diag.Diagnostics {
	nd := n.(*mwNode)
	if nd.name != "" {
		// Named constructors build on first reference.
		return nil
	}
	r := g.Singleton(routerPath, "r", g.Import(routerPath)+".New()")
	expr, ds := m.expr(g, nd)
	g.Node(&gen.Call{
		Base: gen.Base{Phase: gen.PhaseMiddleware, Origin: gen.Origin{Pos: nd.pos}},
		Fn:   r + ".Use",
		Args: []string{expr},
	})
	return ds
}

// expr renders one middleware reference and builds constructors once.
func (m *Middleware) expr(g *gen.Gen, nd *mwNode) (string, diag.Diagnostics) {
	if !nd.ctor {
		return g.ImportPkg(nd.pkg) + "." + nd.fn, nil
	}
	if nd.varName != "" {
		return nd.varName, nil
	}
	var ds diag.Diagnostics
	args := make([]string, 0, len(nd.params))
	for _, pr := range nd.params {
		expr, ids, ok := g.Instance(pr.t, "")
		ds = append(ds, ids...)
		if !ok && len(ids) == 0 {
			help := "declare a //fabrik:provider for it"
			if h, hinted := g.MissingHint(pr.t); hinted {
				help = h
			}
			ds.Error(pr.pos, "no provider or binding supplies this middleware constructor parameter", help)
		}
		args = append(args, expr)
	}
	base := nd.name
	if base == "" {
		base = gen.LowerFirst(nd.fn)
	}
	v := g.Var(base + "MW")
	errStyle := gen.ErrNone
	if nd.errResult {
		errStyle = gen.ErrReturn
	}
	g.Node(&gen.Call{
		Base: gen.Base{Phase: gen.PhaseMiddleware, Origin: gen.Origin{Pos: nd.pos}},
		Var:  v,
		Fn:   g.ImportPkg(nd.pkg) + "." + nd.fn,
		Args: args,
		Err:  errStyle,
	})
	nd.varName = v
	return v, ds
}

// Validate warns about unreferenced named middleware.
func (m *Middleware) Validate(*gen.Gen) diag.Diagnostics {
	var ds diag.Diagnostics
	for _, name := range m.names() {
		if nd := m.byName[name]; !nd.used {
			ds.Warn(nd.pos, fmt.Sprintf("middleware %q is never referenced", name),
				"list it in a middleware= chain, or drop name= to make it global")
		}
	}
	return ds
}

// resolve maps middleware= references to declarations.
func (m *Middleware) resolve(refs []mwRef) ([]*mwNode, diag.Diagnostics) {
	var out []*mwNode
	var ds diag.Diagnostics
	for _, ref := range refs {
		nd := m.byName[ref.name]
		if nd == nil {
			help := "declare one: //fabrik:http:middleware name=" + ref.name + " on a middleware function or constructor"
			if names := m.names(); len(names) > 0 {
				help = "declared names: " + strings.Join(names, ", ")
			}
			ds.Error(ref.pos, fmt.Sprintf("unknown middleware %q", ref.name), help)
			continue
		}
		nd.used = true
		out = append(out, nd)
	}
	return out, ds
}

// names returns the declared middleware names, sorted.
func (m *Middleware) names() []string {
	names := make([]string, 0, len(m.byName))
	for name := range m.byName {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// isMiddlewareSignature reports whether sig is func(http.Handler) http.Handler.
func isMiddlewareSignature(sig *types.Signature) bool {
	return sig.Params().Len() == 1 && sig.Results().Len() == 1 && !sig.Variadic() &&
		types.TypeString(sig.Params().At(0).Type(), nil) == "net/http.Handler" &&
		types.TypeString(sig.Results().At(0).Type(), nil) == "net/http.Handler"
}

// isCtorSignature reports whether sig is a middleware constructor:
// binding-resolved parameters, returning a middleware-typed value,
// optionally with a trailing error.
func isCtorSignature(sig *types.Signature) bool {
	n := sig.Results().Len()
	if n < 1 || n > 2 || sig.Variadic() {
		return false
	}
	if !isMiddlewareType(sig.Results().At(0).Type()) {
		return false
	}
	return n == 1 || isErrorType(sig.Results().At(1).Type())
}

// isMiddlewareType accepts values usable as router.Middleware without
// generated conversions.
func isMiddlewareType(t types.Type) bool {
	t = types.Unalias(t)
	if types.TypeString(t, nil) == routerPath+".Middleware" {
		return true
	}
	sig, ok := t.(*types.Signature)
	if !ok {
		return false
	}
	return sig.Params().Len() == 1 && sig.Results().Len() == 1 && !sig.Variadic() &&
		types.TypeString(sig.Params().At(0).Type(), nil) == "net/http.Handler" &&
		types.TypeString(sig.Results().At(0).Type(), nil) == "net/http.Handler"
}
