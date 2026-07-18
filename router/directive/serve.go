package directive

import (
	"fmt"
	"go/token"
	"go/types"
	"strings"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

// Serve is the //fabrik:http:server directive.
type Serve struct {
	node  *serveNode
	first *token.Position
}

// NewServe returns a Serve directive for one run.
func NewServe() *Serve { return &Serve{} }

func (*Serve) Name() string { return "http:server" }

func (*Serve) Meta() gen.Meta {
	return gen.Meta{
		Synopsis: "Replace the default server startup",
		Doc: "**`//fabrik:http:server`**\n\n" +
			"Marks the function that serves the app, replacing the generated " +
			"`http.ListenAndServe` block. One per app. Parameters may be " +
			"`http.Handler` or `*router.Router` (both receive the router) and " +
			"`context.Context` (the signal-bound root: take it and drain on it for " +
			"graceful shutdown, bounding `Shutdown` with a timeout such as a " +
			"`config.Duration` field); it must return `error`. On a method, the " +
			"receiver struct is wired, so configuration arrives as fields.\n\n" +
			"```go\n//fabrik:http:server\nfunc Serve(ctx context.Context, h http.Handler) error {\n" +
			"\tsrv := &http.Server{Addr: \":8080\", Handler: h}\n" +
			"\terrc := make(chan error, 1)\n" +
			"\tgo func() { errc <- srv.ListenAndServe() }()\n" +
			"\tselect {\n\tcase err := <-errc:\n\t\treturn err\n\tcase <-ctx.Done():\n\t}\n" +
			"\treturn srv.Shutdown(context.Background())\n}\n```",
		Example: "//fabrik:http:server",
	}
}

type serveParam int

const (
	paramRouter serveParam = iota
	paramCtx
)

type serveNode struct {
	pos token.Position

	fn     string
	pkg    *types.Package
	recv   types.Type
	params []serveParam
	fset   *token.FileSet
}

func (s *Serve) Parse(a gen.Annotation) (any, diag.Diagnostics) {
	_, ds := gen.ParseArgs(a, s.Meta())
	if ds.HasFatal() {
		return nil, ds
	}
	return &serveNode{pos: a.Pos}, ds
}

func (s *Serve) Check(n any, t gen.Typed) diag.Diagnostics {
	nd := n.(*serveNode)
	var ds diag.Diagnostics

	fn, ok := t.Target.(*types.Func)
	if !ok {
		ds.Error(nd.pos, "//fabrik:http:server must be on a function", "")
		return ds
	}
	if isGenericFunc(fn) {
		ds.Error(nd.pos, fmt.Sprintf("serve function %s cannot be generic (generated code cannot instantiate type parameters)", fn.Name()),
			"declare a concrete function")
		return ds
	}
	sig := fn.Signature()
	if recv := sig.Recv(); recv != nil {
		if !isNamedStruct(recv.Type()) {
			ds.Error(nd.pos, fmt.Sprintf("serve receiver %s is not a struct", types.TypeString(recv.Type(), types.RelativeTo(fn.Pkg()))),
				"//fabrik:http:server methods must be on a struct")
			return ds
		}
		nd.recv = recv.Type()
	}
	if sig.Results().Len() != 1 || !isErrorType(sig.Results().At(0).Type()) {
		ds.Error(nd.pos, fmt.Sprintf("serve function %s must return error", fn.Name()),
			"example: func Serve(h http.Handler) error")
		return ds
	}
	for i := 0; i < sig.Params().Len(); i++ {
		p := sig.Params().At(i)
		switch types.TypeString(types.Unalias(p.Type()), nil) {
		case "net/http.Handler", "*" + routerPath + ".Router":
			nd.params = append(nd.params, paramRouter)
		case "context.Context":
			nd.params = append(nd.params, paramCtx)
		default:
			ds.Error(t.Fset.Position(p.Pos()),
				fmt.Sprintf("serve parameter %s must be http.Handler, *router.Router, or context.Context", p.Name()),
				"other dependencies arrive as fields of the receiver struct")
			return ds
		}
	}
	if s.first != nil {
		ds.Error(nd.pos, "duplicate //fabrik:http:server",
			fmt.Sprintf("first declared at %s", *s.first))
		return ds
	}
	s.first = &nd.pos

	nd.fn = fn.Name()
	nd.pkg = fn.Pkg()
	nd.fset = t.Fset
	return ds
}

// Emit defers serving until all route directives have emitted.
func (s *Serve) Emit(n any, g *gen.Gen) diag.Diagnostics {
	s.node = n.(*serveNode)
	return nil
}

// Finish writes the serve block when a router exists.
func (s *Serve) Finish(g *gen.Gen) diag.Diagnostics {
	if s.node == nil && !g.HasSingleton(routerPath) {
		return nil
	}

	if s.node == nil {
		r := g.Singleton(routerPath, "r", g.Import(routerPath)+".New()")
		osPkg := g.Import("os")
		httpPkg := g.Import("net/http")
		ctx := g.Context()
		// The default server depends on the signal-bound root context.
		g.RequireShutdownEnvelope()
		ctxPkg := g.Import("context")
		errPkg := g.Import("errors")
		timePkg := g.Import("time")
		srv := g.Var("srv")
		g.Node(&gen.Raw{
			Base:    gen.Base{Phase: gen.PhaseServe},
			Lines:   []string{`addr := ":8080"`},
			Defines: []string{"addr"},
		})
		g.Node(&gen.Raw{
			Base:  gen.Base{Phase: gen.PhaseServe},
			Lines: []string{fmt.Sprintf(`if p := %s.Getenv("PORT"); p != "" {`, osPkg), `addr = ":" + p`, `}`},
		})
		g.Node(&gen.Raw{
			Base: gen.Base{Phase: gen.PhaseServe},
			Lines: []string{
				fmt.Sprintf("%s := &%s.Server{Addr: addr, Handler: %s}", srv, httpPkg, r),
				"errc := make(chan error, 1)",
				fmt.Sprintf("go func() { errc <- %s.ListenAndServe() }()", srv),
				"select {",
				"case err := <-errc:",
				fmt.Sprintf("if %s.Is(err, %s.ErrServerClosed) {", errPkg, httpPkg),
				"return nil",
				"}",
				"return err",
				fmt.Sprintf("case <-%s.Done():", ctx),
				"}",
				fmt.Sprintf("shutdownCtx, shutdownCancel := %s.WithTimeout(%s.Background(), 10*%s.Second)", ctxPkg, ctxPkg, timePkg),
				"defer shutdownCancel()",
				fmt.Sprintf("return %s.Shutdown(shutdownCtx)", srv),
			},
			Defines: []string{srv},
		})
		return nil
	}

	nd := s.node
	hasCtx := false
	for _, p := range nd.params {
		if p == paramCtx {
			hasCtx = true
		}
	}
	// A server that cannot observe the root context would prevent coordinated shutdown.
	if !hasCtx && g.EnvelopeActive() {
		var ds diag.Diagnostics
		ds.Error(nd.pos, fmt.Sprintf("//fabrik:http:server %s must accept a context.Context", nd.fn),
			"this app hosts a background runtime whose graceful shutdown is coordinated on a signal-bound context; add the parameter: func Serve(ctx context.Context, h http.Handler) error")
		return ds
	}
	callee, ds := handlerExpr(g, nd.recv, nd.pkg, nd.fn, nd.fset)
	args := make([]string, 0, len(nd.params))
	for _, p := range nd.params {
		switch p {
		case paramRouter:
			// Create the router only when the serve function accepts it.
			args = append(args, g.Singleton(routerPath, "r", g.Import(routerPath)+".New()"))
		case paramCtx:
			args = append(args, g.Context())
			g.RequireShutdownEnvelope()
		}
	}
	g.Node(&gen.Serve{
		Base: gen.Base{Phase: gen.PhaseServe, Origin: gen.Origin{Pos: nd.pos}},
		Expr: callee + "(" + strings.Join(args, ", ") + ")",
	})
	return ds
}

// isErrorType reports whether t is the built-in error type.
func isErrorType(t types.Type) bool {
	return types.TypeString(types.Unalias(t), nil) == "error"
}

// PrepareNode registers the serve function's receiver struct before resolution.
func (s *Serve) PrepareNode(n any, g *gen.Gen) {
	nd := n.(*serveNode)
	prepareReceiver(g, nd.recv, nd.fset)
}
