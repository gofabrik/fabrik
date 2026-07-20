package directive

import (
	"fmt"
	"go/token"
	"go/types"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

// Hook implements a router miss-handler directive.
type Hook struct {
	host   *Host
	name   string // directive name
	method string // router method to emit
	first  *token.Position
}

// NewNotFound returns the //fabrik:http:notfound directive for one run.
func NewNotFound(host *Host) *Hook {
	return &Hook{host: host, name: "http:notfound", method: "NotFound"}
}

// NewMethodNotAllowed returns the //fabrik:http:methodnotallowed directive.
func NewMethodNotAllowed(host *Host) *Hook {
	return &Hook{host: host, name: "http:methodnotallowed", method: "MethodNotAllowed"}
}

func (h *Hook) Name() string { return h.name }

func (h *Hook) Meta() gen.Meta {
	doc := map[string]string{
		"http:notfound": "**`//fabrik:http:notfound`**\n\n" +
			"Sets the handler for requests that match no route. One per app. " +
			"Standard handler signature; the response defaults to 404.\n\n" +
			"```go\n//fabrik:http:notfound\nfunc NotFound(w http.ResponseWriter, r *http.Request) { ... }\n```",
		"http:methodnotallowed": "**`//fabrik:http:methodnotallowed`**\n\n" +
			"Sets the handler for requests whose path matches a route under a " +
			"different method. One per app. The Allow header is set before it " +
			"runs and the response defaults to 405.\n\n" +
			"```go\n//fabrik:http:methodnotallowed\nfunc MethodNotAllowed(w http.ResponseWriter, r *http.Request) { ... }\n```",
	}
	synopsis := map[string]string{
		"http:notfound":         "Handler for requests matching no route",
		"http:methodnotallowed": "Handler for method mismatches (405)",
	}
	return gen.Meta{
		Synopsis: synopsis[h.name],
		Doc:      doc[h.name],
		Example:  "//fabrik:" + h.name,
		Tier:     gen.TierBind,
	}
}

type hookNode struct {
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
	return &hookNode{pos: a.Pos}, ds
}

func (h *Hook) Check(n any, t gen.Typed) diag.Diagnostics {
	nd := n.(*hookNode)
	var ds diag.Diagnostics

	fn, ok := t.Target.(*types.Func)
	if !ok {
		ds.Error(nd.pos, "//fabrik:"+h.name+" must be on a function", "")
		return ds
	}
	if isGenericFunc(fn) {
		ds.Error(nd.pos, fmt.Sprintf("handler %s cannot be generic (generated code cannot instantiate type parameters)", fn.Name()),
			"declare a concrete handler")
		return ds
	}
	sig := fn.Signature()
	if !isHandlerSignature(sig) {
		ds.Error(nd.pos, fmt.Sprintf("handler %s has the wrong signature", fn.Name()),
			"want func(w http.ResponseWriter, r *http.Request)")
		return ds
	}
	if recv := sig.Recv(); recv != nil {
		if !isNamedStruct(recv.Type()) {
			ds.Error(nd.pos, fmt.Sprintf("handler receiver %s is not a struct", types.TypeString(recv.Type(), types.RelativeTo(fn.Pkg()))),
				"//fabrik:"+h.name+" handlers must be methods on a struct")
			return ds
		}
		nd.recv = recv.Type()
	}
	if h.first != nil {
		ds.Error(nd.pos, fmt.Sprintf("duplicate //fabrik:%s", h.name),
			fmt.Sprintf("first declared at %s", *h.first))
		return ds
	}
	h.first = &nd.pos

	nd.fn = fn.Name()
	nd.pkg = fn.Pkg()
	nd.fset = t.Fset
	return ds
}

func (h *Hook) Emit(n any, g *gen.Gen) diag.Diagnostics {
	nd := n.(*hookNode)
	h.host.record(func(g *gen.Gen) diag.Diagnostics {
		r := g.Singleton(routerPath, "r", g.Import(routerPath)+".New()")
		handler, ds := handlerExpr(g, nd.recv, nd.pkg, nd.fn, nd.fset)
		g.Node(&gen.Call{
			Base: gen.Base{Phase: gen.PhaseRegister, Origin: gen.Origin{Pos: nd.pos}},
			Fn:   r + "." + h.method,
			Args: []string{handler},
		})
		return ds
	})
	return nil
}

// PrepareNode registers the hook's receiver struct before resolution.
func (h *Hook) PrepareNode(n any, g *gen.Gen) {
	nd := n.(*hookNode)
	h.host.prepareReceiver(g, nd.recv, nd.fset)
}
