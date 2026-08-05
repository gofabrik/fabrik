package directive

import (
	"fmt"
	"go/token"
	"go/types"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

// Hook implements a typed router miss-handler directive: the web counterpart
// of //fabrik:http:notfound and //fabrik:http:methodnotallowed.
type Hook struct {
	web    *Web
	name   string // directive name
	method string // router method to emit
}

// NewNotFound returns the //fabrik:web:notfound directive for one run.
func (w *Web) NewNotFound() *Hook {
	return &Hook{web: w, name: "web:notfound", method: "NotFound"}
}

// NewMethodNotAllowed returns the //fabrik:web:methodnotallowed directive.
func (w *Web) NewMethodNotAllowed() *Hook {
	return &Hook{web: w, name: "web:methodnotallowed", method: "MethodNotAllowed"}
}

func (h *Hook) Name() string { return h.name }

func (h *Hook) Meta() gen.Meta {
	doc := map[string]string{
		"web:notfound": "**`//fabrik:web:notfound`**\n\n" +
			"Sets the typed handler for requests that match no route. One per " +
			"app, exclusive with `//fabrik:http:notfound`. The handler renders " +
			"through the app's adapter; a response that sets no status keeps " +
			"the 404.\n\n" +
			"```go\n//fabrik:web:notfound\nfunc (e *ErrorPages) NotFound(req *web.Request) (web.Response, error) { ... }\n```",
		"web:methodnotallowed": "**`//fabrik:web:methodnotallowed`**\n\n" +
			"Sets the typed handler for requests whose path matches a route " +
			"under a different method. One per app, exclusive with " +
			"`//fabrik:http:methodnotallowed`. The Allow header is set before " +
			"it runs; a response that sets no status keeps the 405.\n\n" +
			"```go\n//fabrik:web:methodnotallowed\nfunc (e *ErrorPages) MethodNotAllowed(req *web.Request) (web.Response, error) { ... }\n```",
	}
	synopsis := map[string]string{
		"web:notfound":         "Typed handler for requests matching no route",
		"web:methodnotallowed": "Typed handler for method mismatches (405)",
	}
	return gen.Meta{
		Synopsis: synopsis[h.name],
		Doc:      doc[h.name],
		Example:  "//fabrik:" + h.name,
		Tier:     gen.TierBind,
	}
}

type webHookNode struct {
	pos token.Position

	fn   string
	pkg  *types.Package
	recv types.Type
	fset *token.FileSet
}

func (h *Hook) Parse(a gen.Annotation) (any, diag.Diagnostics) {
	_, ds := gen.ParseArgs(a, h.Meta())
	if ds.HasFatal() {
		return nil, ds
	}
	return &webHookNode{pos: a.Pos}, ds
}

func (h *Hook) Check(n any, t gen.Typed) diag.Diagnostics {
	nd := n.(*webHookNode)
	var ds diag.Diagnostics

	fn, ok := t.Target.(*types.Func)
	if !ok {
		ds.Error(nd.pos, "//fabrik:"+h.name+" must be on a function", "")
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
		if _, ok := h.web.host.ReceiverInfo(recv.Type()); !ok {
			ds.Error(nd.pos, fmt.Sprintf("handler receiver %s is not a struct", types.TypeString(recv.Type(), types.RelativeTo(fn.Pkg()))),
				"//fabrik:"+h.name+" handlers must be methods on a struct")
			return ds
		}
		nd.recv = recv.Type()
	}
	if first, ok := h.web.host.ClaimHook(h.method, h.name, nd.pos); !ok {
		if first.Directive == h.name {
			ds.Error(nd.pos, fmt.Sprintf("duplicate //fabrik:%s", h.name),
				fmt.Sprintf("first declared at %s", first.Pos))
		} else {
			ds.Error(nd.pos, fmt.Sprintf("//fabrik:%s conflicts with //fabrik:%s at %s", h.name, first.Directive, first.Pos),
				"the router holds one handler per hook; declare one of the two")
		}
		return ds
	}

	nd.fn = fn.Name()
	nd.pkg = fn.Pkg()
	nd.fset = t.Fset
	return ds
}

// Emit registers the adapter-wrapped handler on the router's miss hook.
func (h *Hook) Emit(n any, g *gen.Gen) diag.Diagnostics {
	nd := n.(*webHookNode)
	if nd.fn == "" {
		return nil
	}
	h.web.ensureAdapter(g, nd.pos)
	h.web.host.EmitHook(g, h.method, nd.pos, func(g *gen.Gen) (string, diag.Diagnostics) {
		handler, ds := h.web.host.HandlerExpr(g, nd.recv, nd.pkg, nd.fn, nd.fset)
		adapter, ads, ok := g.InstancePath(adapterPath)
		ds = append(ds, ads...)
		if !ok {
			return "nil", ds
		}
		return adapter + ".Wrap(" + handler + ")", ds
	})
	return nil
}

// PrepareNode registers the handler's receiver struct before resolution.
func (h *Hook) PrepareNode(n any, g *gen.Gen) {
	nd := n.(*webHookNode)
	if nd.recv != nil {
		h.web.host.PrepareReceiver(g, nd.recv, nd.fset)
	}
}
