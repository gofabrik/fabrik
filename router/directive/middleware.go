package directive

import (
	"fmt"
	"go/token"
	"go/types"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

// Middleware is the //fabrik:http:middleware directive.
type Middleware struct{}

// NewMiddleware returns a Middleware directive for one run.
func NewMiddleware() *Middleware { return &Middleware{} }

func (*Middleware) Name() string { return "http:middleware" }

func (*Middleware) Meta() gen.Meta {
	return gen.Meta{
		Synopsis: "Global middleware for every route",
		Doc: "**`//fabrik:http:middleware`**\n\n" +
			"Registers a global middleware, applied to every request including " +
			"404/405, in source order. For per-route middleware, omit the " +
			"directive and reference the function with `middleware=` on the " +
			"route instead.\n\n" +
			"```go\n//fabrik:http:middleware\nfunc RequestID(next http.Handler) http.Handler { ... }\n```",
		Example: "//fabrik:http:middleware",
	}
}

type mwNode struct {
	pos token.Position

	fn  string
	pkg *types.Package
}

func (m *Middleware) Parse(a gen.Annotation) (any, diag.Diagnostics) {
	_, ds := gen.ParseArgs(a, m.Meta())
	if ds.HasFatal() {
		return nil, ds
	}
	return &mwNode{pos: a.Pos}, ds
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

	nd.fn = fn.Name()
	nd.pkg = fn.Pkg()
	return ds
}

func (m *Middleware) Emit(n any, g *gen.Gen) diag.Diagnostics {
	nd := n.(*mwNode)
	r := g.Singleton(routerPath, "r", g.Import(routerPath)+".New()")
	g.Stmt(gen.PhaseMiddleware, "%s.Use(%s.%s)", r, g.ImportPkg(nd.pkg), nd.fn)
	return nil
}

// isMiddlewareSignature reports whether sig is func(http.Handler) http.Handler.
func isMiddlewareSignature(sig *types.Signature) bool {
	return sig.Params().Len() == 1 && sig.Results().Len() == 1 && !sig.Variadic() &&
		types.TypeString(sig.Params().At(0).Type(), nil) == "net/http.Handler" &&
		types.TypeString(sig.Results().At(0).Type(), nil) == "net/http.Handler"
}
