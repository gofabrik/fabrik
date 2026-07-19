// Package directive provides //fabrik:web.
package directive

import (
	"fmt"
	"go/token"
	"go/types"

	routerdir "github.com/gofabrik/fabrik/router/directive"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

// Path strings keep signature checks independent of runtime imports.
const (
	webPath      = "github.com/gofabrik/fabrik/web"
	requestPath  = "*" + webPath + ".Request"
	responsePath = webPath + ".Response"
	adapterPath  = "*" + webPath + ".Adapter"

	templatesSetPath = "*github.com/gofabrik/fabrik/templates.Set"
)

// Web is the //fabrik:web directive.
type Web struct {
	host       *routerdir.Host
	registered bool
}

// NewWeb returns a Web directive for one run.
func NewWeb(host *routerdir.Host) *Web {
	return &Web{host: host}
}

func (*Web) Name() string { return "web" }

func (*Web) Meta() gen.Meta {
	return gen.Meta{
		Synopsis: "Typed-response route: METHOD /path [middleware=a,b]",
		Doc: "**`//fabrik:web METHOD /path [middleware=name,name2]`**\n\n" +
			"Registers a typed-response handler: `func(*web.Request) " +
			"(web.Response, error)` - request in, response value out, " +
			"errors centralized in the generated adapter. Same grammar, groups, middleware " +
			"names, and conflict table as `//fabrik:http`; typed and plain " +
			"handlers mix freely, even on one struct. When " +
			"`//fabrik:templates` is declared, `web.View` responses render " +
			"through the app's template set.\n\n" +
			"```go\n//fabrik:web POST /login\nfunc (h *Handlers) Login(req *web.Request) (web.Response, error) { ... }\n```",
		Example: "//fabrik:web GET /login",
		Pos: []gen.PosSpec{
			{Name: "METHOD", Kind: gen.KindFreeform, Values: []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"}},
			{Name: "PATH", Kind: gen.KindFreeform},
		},
		Attrs: []gen.AttrSpec{
			{Key: "middleware", Kind: gen.KindMiddlewareRef},
		},
	}
}

type webNode struct {
	args routerdir.RouteArgs
	pos  token.Position

	fn      string
	pkg     *types.Package
	recv    types.Type
	recvObj *types.TypeName
	fset    *token.FileSet
}

func (w *Web) Parse(a gen.Annotation) (any, diag.Diagnostics) {
	args, ds := w.host.ParseRoute(a, w.Meta())
	if args.Method == "" && args.Path == "" {
		return nil, ds
	}
	if ds.HasFatal() {
		return nil, ds
	}
	return &webNode{args: args, pos: a.Pos}, ds
}

func (w *Web) Check(n any, t gen.Typed) diag.Diagnostics {
	nd := n.(*webNode)
	var ds diag.Diagnostics

	fn, ok := t.Target.(*types.Func)
	if !ok {
		ds.Error(nd.pos, "//fabrik:web must be on a function", "")
		return ds
	}
	sig := fn.Signature()
	if sig.TypeParams().Len() > 0 || sig.RecvTypeParams().Len() > 0 {
		ds.Error(nd.pos, fmt.Sprintf("handler %s cannot be generic (generated code cannot instantiate type parameters)", fn.Name()),
			"declare a concrete handler")
		return ds
	}
	if !isWebSignature(sig) {
		ds.Error(nd.pos, fmt.Sprintf("handler %s has the wrong signature", fn.Name()),
			"want func(req *web.Request) (web.Response, error)")
		return ds
	}
	if recv := sig.Recv(); recv != nil {
		obj, ok := w.host.ReceiverInfo(recv.Type())
		if !ok {
			ds.Error(nd.pos, fmt.Sprintf("handler receiver %s is not a struct", types.TypeString(recv.Type(), types.RelativeTo(fn.Pkg()))),
				"//fabrik:web handlers must be methods on a struct")
			return ds
		}
		nd.recv = recv.Type()
		nd.recvObj = obj
	}

	nd.fn = fn.Name()
	nd.pkg = fn.Pkg()
	nd.fset = t.Fset
	return ds
}

// isWebSignature reports whether sig is
// func(*web.Request) (web.Response, error).
func isWebSignature(sig *types.Signature) bool {
	p, res := sig.Params(), sig.Results()
	return p.Len() == 1 && res.Len() == 2 && !sig.Variadic() &&
		typePath(p.At(0).Type()) == requestPath &&
		typePath(res.At(0).Type()) == responsePath &&
		typePath(res.At(1).Type()) == "error"
}

// typePath unwraps aliases through pointers for path-based signature checks.
func typePath(t types.Type) string {
	t = types.Unalias(t)
	if p, ok := t.(*types.Pointer); ok {
		return "*" + typePath(p.Elem())
	}
	return types.TypeString(t, nil)
}

// Emit registers the shared adapter binding and route.
func (w *Web) Emit(n any, g *gen.Gen) diag.Diagnostics {
	nd := n.(*webNode)
	if nd.fn == "" {
		return nil
	}
	if !w.registered {
		w.registered = true
		g.BindLazyPath(adapterPath, func() (string, diag.Diagnostics) {
			var ds diag.Diagnostics
			webPkg := g.Import(webPath)
			var args []string
			// Attach the template set only when one is declared.
			if g.HasBindingPath(templatesSetPath) {
				expr, ids, ok := g.InstancePath(templatesSetPath)
				ds = append(ds, ids...)
				if ok && len(ids) == 0 {
					args = append(args, webPkg+".WithRenderer("+expr+")")
				}
			}
			v := g.Var("adapter")
			g.Node(&gen.Call{
				Base: gen.Base{Phase: gen.PhaseWire},
				Var:  v,
				Fn:   webPkg + ".NewAdapter",
				Args: args,
			})
			return v, ds
		})
	}
	return w.host.EmitRoute(g, nd.args, nd.recvObj, nd.pos, func() (string, diag.Diagnostics) {
		handler, ds := w.host.HandlerExpr(g, nd.recv, nd.pkg, nd.fn, nd.fset)
		adapter, ads, ok := g.InstancePath(adapterPath)
		ds = append(ds, ads...)
		if !ok {
			return "nil", ds
		}
		return adapter + ".Wrap(" + handler + ")", ds
	})
}

// PrepareNode registers the handler's receiver struct before resolution.
func (w *Web) PrepareNode(n any, g *gen.Gen) {
	nd := n.(*webNode)
	if nd.recv != nil {
		w.host.PrepareReceiver(g, nd.recv, nd.fset)
	}
	routerdir.BindHTTPServer(g)
}
