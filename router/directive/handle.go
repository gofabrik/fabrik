package directive

import (
	"fmt"
	"go/token"
	"go/types"
	"strings"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

// Handle is the //fabrik:http:handle directive.
type Handle struct {
	groups *Group
	routes *routeTable
	mw     *Middleware
}

// NewHandle returns a Handle directive for one run.
func NewHandle(groups *Group, routes *routeTable, mw *Middleware) *Handle {
	return &Handle{groups: groups, routes: routes, mw: mw}
}

func (*Handle) Name() string { return "http:handle" }

func (*Handle) Meta() gen.Meta {
	return gen.Meta{
		Synopsis: "Method-agnostic handler: /path [middleware=a,b]",
		Doc: "**`//fabrik:http:handle /path [middleware=name,name2]`**\n\n" +
			"Registers a handler for every method of a pattern. Two shapes: a " +
			"standard handler func, or a function without parameters returning " +
			"`http.Handler`, called once at startup - the escape hatch for " +
			"third-party handlers. `middleware=` wraps the route in a " +
			"comma-separated chain of declared names, same as on `//fabrik:http`.\n\n" +
			"```go\n//fabrik:http:handle /metrics\nfunc Metrics() http.Handler { return promhttp.Handler() }\n```",
		Example: "//fabrik:http:handle /metrics",
		Pos: []gen.PosSpec{
			{Name: "PATH", Kind: gen.KindFreeform},
		},
		Attrs: []gen.AttrSpec{
			{Key: "middleware", Kind: gen.KindMiddlewareRef},
		},
	}
}

type handleNode struct {
	path string
	pos  token.Position
	refs []mwRef

	fn       string
	pkg      *types.Package
	recv     types.Type
	recvObj  *types.TypeName
	produces bool // func() http.Handler shape
	fset     *token.FileSet
}

func (h *Handle) Parse(a gen.Annotation) (any, diag.Diagnostics) {
	args, ds := gen.ParseArgs(a, h.Meta())
	if len(args.Pos) < 1 {
		return nil, ds
	}
	path := args.Pos[0]
	if !strings.HasPrefix(path.Text, "/") {
		ds.Error(a.ArgPos(path.Col), fmt.Sprintf("handle path must start with %q (got %q)", "/", path.Text),
			"example: //fabrik:http:handle /metrics")
	} else if pe := patternError(path.Text); pe != "" {
		ds.Error(a.ArgPos(path.Col), "invalid route pattern: "+pe,
			"wildcards: /{name}, /{name...} (rest of path, last), /{$} (exact match, last)")
	}
	nd := &handleNode{path: path.Text, pos: a.Pos}
	if mw, ok := args.Attr["middleware"]; ok {
		refs, rds := parseMWRefs(a, mw)
		ds = append(ds, rds...)
		nd.refs = refs
	}
	if ds.HasFatal() {
		return nil, ds
	}
	return nd, ds
}

func (h *Handle) Check(n any, t gen.Typed) diag.Diagnostics {
	nd := n.(*handleNode)
	var ds diag.Diagnostics

	fn, ok := t.Target.(*types.Func)
	if !ok {
		ds.Error(nd.pos, "//fabrik:http:handle must be on a function", "")
		return ds
	}
	if isGenericFunc(fn) {
		ds.Error(nd.pos, fmt.Sprintf("handler %s cannot be generic (generated code cannot instantiate type parameters)", fn.Name()),
			"declare a concrete handler")
		return ds
	}
	sig := fn.Signature()
	switch {
	case isHandlerSignature(sig):
	case producesHandler(sig):
		nd.produces = true
	default:
		ds.Error(nd.pos, fmt.Sprintf("handler %s has the wrong signature", fn.Name()),
			"want func(w http.ResponseWriter, r *http.Request), or func() http.Handler")
		return ds
	}
	if recv := sig.Recv(); recv != nil {
		if !isNamedStruct(recv.Type()) {
			ds.Error(nd.pos, fmt.Sprintf("handler receiver %s is not a struct", types.TypeString(recv.Type(), types.RelativeTo(fn.Pkg()))),
				"//fabrik:http:handle handlers must be methods on a struct")
			return ds
		}
		nd.recv = recv.Type()
		nd.recvObj = namedOf(recv.Type()).Obj()
	}

	nd.fn = fn.Name()
	nd.pkg = fn.Pkg()
	nd.fset = t.Fset
	return ds
}

func (h *Handle) Emit(n any, g *gen.Gen) diag.Diagnostics {
	nd := n.(*handleNode)
	var ds diag.Diagnostics

	pattern, refs := effectiveRoute(h.groups, nd.recvObj, nd.path, nd.refs)

	// Duplicate routes still validate middleware references.
	mws, mds := h.mw.resolve(refs)
	ds = append(ds, mds...)

	if d, ok := h.routes.add(pattern, nd.pos); !ok {
		return append(ds, d)
	}

	r := g.Singleton(routerPath, "r", g.Import(routerPath)+".New()")
	callee, hds := handlerExpr(g, nd.recv, nd.pkg, nd.fn, nd.fset)
	ds = append(ds, hds...)

	// Middleware and produced handlers require http.Handler registration.
	if !nd.produces && len(mws) == 0 {
		g.Stmt(gen.PhaseRegister, "%s.HandleFunc(%q, %s)", r, pattern, callee)
		return ds
	}
	expr := callee + "()"
	if !nd.produces {
		expr = g.Import("net/http") + ".HandlerFunc(" + callee + ")"
	}
	for i := len(mws) - 1; i >= 0; i-- {
		expr = g.ImportPkg(mws[i].pkg) + "." + mws[i].fn + "(" + expr + ")"
	}
	g.Stmt(gen.PhaseRegister, "%s.Handle(%q, %s)", r, pattern, expr)
	return ds
}

// producesHandler reports whether sig is func() http.Handler.
func producesHandler(sig *types.Signature) bool {
	return sig.Params().Len() == 0 && sig.Results().Len() == 1 && !sig.Variadic() &&
		types.TypeString(sig.Results().At(0).Type(), nil) == "net/http.Handler"
}

// PrepareNode registers the handler's receiver struct before resolution.
func (h *Handle) PrepareNode(n any, g *gen.Gen) {
	nd := n.(*handleNode)
	prepareReceiver(g, nd.recv, nd.fset)
}
