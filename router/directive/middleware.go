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
		Synopsis: "Middleware: global, or named for middleware= chains [name=NAME]",
		Doc: "**`//fabrik:http:middleware [name=NAME]`**\n\n" +
			"On a `func(next http.Handler) http.Handler`. Bare, it registers " +
			"a global middleware, applied to every request including " +
			"404/405, in source order. With `name=`, it is not global: " +
			"routes and groups opt in by listing the name in their " +
			"`middleware=` chain - directives reference declared names, " +
			"never code.\n\n" +
			"```go\n//fabrik:http:middleware name=auth\nfunc RequireAuth(next http.Handler) http.Handler { ... }\n```",
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
	if !isMiddlewareSignature(sig) {
		ds.Error(nd.pos, fmt.Sprintf("middleware %s has the wrong signature", fn.Name()),
			"want func(next http.Handler) http.Handler")
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
		// Named middleware applies only where a middleware= chain lists it.
		return nil
	}
	r := g.Singleton(routerPath, "r", g.Import(routerPath)+".New()")
	g.Stmt(gen.PhaseMiddleware, "%s.Use(%s.%s)", r, g.ImportPkg(nd.pkg), nd.fn)
	return nil
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
			help := "declare one: //fabrik:http:middleware name=" + ref.name + " on a func(next http.Handler) http.Handler"
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
