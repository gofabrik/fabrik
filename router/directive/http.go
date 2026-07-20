// Package directive implements fabrik router directives.
package directive

import (
	"fmt"
	"go/token"
	"go/types"
	"strings"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

const routerPath = "github.com/gofabrik/fabrik/router"

var httpMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"}

// HTTP is the //fabrik:http directive.
type HTTP struct {
	host *Host
}

// NewHTTP returns an HTTP directive for one run.
func NewHTTP(host *Host) *HTTP {
	return &HTTP{host: host}
}

func (*HTTP) Name() string { return "http" }

func (*HTTP) Meta() gen.Meta {
	return gen.Meta{
		Synopsis: "HTTP route: METHOD /path [middleware=a,b]",
		Doc: "**`//fabrik:http METHOD /path [middleware=name,name2]`**\n\n" +
			"Registers a standard `net/http` handler on the fabrik router. " +
			"METHOD is any HTTP method token, including extensions like PURGE. " +
			"Handler signature: `func(w http.ResponseWriter, r *http.Request)`, " +
			"on a plain function or a method of a wired struct. " +
			"`middleware=` wraps this route in a comma-separated chain of " +
			"`//fabrik:http:middleware name=` declarations, outermost first.\n\n" +
			"```go\n//fabrik:http GET /login\n//fabrik:http POST /account middleware=auth\n```",
		Example: "//fabrik:http GET /login",
		Pos: []gen.PosSpec{
			// Values seed completions; Parse accepts any method token.
			{Name: "METHOD", Kind: gen.KindFreeform, Values: httpMethods},
			{Name: "PATH", Kind: gen.KindFreeform},
		},
		Attrs: []gen.AttrSpec{
			{Key: "middleware", Kind: gen.KindMiddlewareRef},
		},
	}
}

type mwRef struct {
	name string
	pos  token.Position
}

type node struct {
	args RouteArgs
	pos  token.Position

	fn      string
	pkg     *types.Package
	recv    types.Type      // nil for a plain function handler
	recvObj *types.TypeName // the receiver's type name, for group lookup
	fset    *token.FileSet
}

func (h *HTTP) Parse(a gen.Annotation) (any, diag.Diagnostics) {
	args, ds := h.host.ParseRoute(a, h.Meta())
	if args.Method == "" && args.Path == "" {
		return nil, ds
	}
	if ds.HasFatal() {
		return nil, ds
	}
	return &node{args: args, pos: a.Pos}, ds
}

func parseMWRefs(a gen.Annotation, mw gen.Arg) ([]mwRef, diag.Diagnostics) {
	var refs []mwRef
	var ds diag.Diagnostics
	offset := 0
	for _, part := range strings.SplitAfter(mw.Text, ",") {
		ref := strings.TrimSuffix(part, ",")
		lead := len(ref) - len(strings.TrimLeft(ref, " \t"))
		ref = strings.TrimSpace(ref)
		pos := a.ArgPos(mw.Col + offset + lead)
		offset += len(part)

		if strings.Contains(ref, ".") {
			ds.Error(pos, fmt.Sprintf("middleware %q references code; middleware= takes declared names", ref),
				"declare //fabrik:http:middleware name=<name> on the function and reference the name")
			continue
		}
		if !isIdentifier(ref) {
			ds.Error(pos, fmt.Sprintf("invalid middleware name %q", ref),
				"use declared names, comma-separated: middleware=auth,audit")
			continue
		}
		refs = append(refs, mwRef{name: ref, pos: pos})
	}
	return refs, ds
}

func isIdentifier(s string) bool {
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b == '_' || (i > 0 && b >= '0' && b <= '9') {
			continue
		}
		return false
	}
	return s != ""
}

func (h *HTTP) Check(n any, t gen.Typed) diag.Diagnostics {
	nd := n.(*node)
	var ds diag.Diagnostics

	fn, ok := t.Target.(*types.Func)
	if !ok {
		ds.Error(nd.pos, "//fabrik:http must be on a function", "")
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
				"//fabrik:http handlers must be methods on a struct")
			return ds
		}
		nd.recv = recv.Type()
		obj, _ := h.host.ReceiverInfo(recv.Type())
		nd.recvObj = obj
	}

	nd.fn = fn.Name()
	nd.pkg = fn.Pkg()
	nd.fset = t.Fset
	return ds
}

func (h *HTTP) Emit(n any, g *gen.Gen) diag.Diagnostics {
	nd := n.(*node)
	return h.host.EmitRoute(g, nd.args, nd.recvObj, nd.pos, func() (string, diag.Diagnostics) {
		return h.host.HandlerExpr(g, nd.recv, nd.pkg, nd.fn, nd.fset)
	})
}

func validMethod(m string) bool {
	if m == "" {
		return false
	}
	for i := 0; i < len(m); i++ {
		c := m[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0 {
			continue
		}
		return false
	}
	return true
}

func isHandlerSignature(sig *types.Signature) bool {
	p := sig.Params()
	return p.Len() == 2 && sig.Results().Len() == 0 && !sig.Variadic() &&
		types.TypeString(p.At(0).Type(), nil) == "net/http.ResponseWriter" &&
		types.TypeString(p.At(1).Type(), nil) == "*net/http.Request"
}

func isGenericFunc(fn *types.Func) bool {
	sig := fn.Signature()
	return sig.TypeParams().Len() > 0 || sig.RecvTypeParams().Len() > 0
}

func isErrorType(t types.Type) bool {
	return types.TypeString(types.Unalias(t), nil) == "error"
}

func isNamedStruct(t types.Type) bool {
	n := namedOf(t)
	if n == nil {
		return false
	}
	_, ok := n.Underlying().(*types.Struct)
	return ok
}

// namedOf unwraps t to its named type, through aliases and one pointer.
func namedOf(t types.Type) *types.Named {
	t = types.Unalias(t)
	if p, ok := t.(*types.Pointer); ok {
		t = types.Unalias(p.Elem())
	}
	n, _ := t.(*types.Named)
	return n
}

// joinPattern places a route under a group prefix; "/{$}" maps to the bare
// prefix, mirroring the router's own rule.
func joinPattern(base, pattern string) string {
	if base != "" && pattern == "/{$}" {
		return base
	}
	return base + pattern
}

// patternError mirrors ServeMux pattern validation.
func patternError(path string) string {
	segs := strings.Split(path[1:], "/")
	for i, seg := range segs {
		openCount, closeCount := strings.Count(seg, "{"), strings.Count(seg, "}")
		if openCount == 0 && closeCount == 0 {
			continue
		}
		if openCount != 1 || closeCount != 1 || !strings.HasPrefix(seg, "{") || !strings.HasSuffix(seg, "}") {
			return fmt.Sprintf("a wildcard must be a full segment (in %q)", seg)
		}
		last := i == len(segs)-1
		name := seg[1 : len(seg)-1]
		switch {
		case name == "$":
			if !last {
				return `"{$}" must be the last segment`
			}
		case strings.HasSuffix(name, "..."):
			if !isIdentifier(strings.TrimSuffix(name, "...")) {
				return fmt.Sprintf("invalid wildcard name in %q", seg)
			}
			if !last {
				return fmt.Sprintf("%q must be the last segment", seg)
			}
		default:
			if !isIdentifier(name) {
				return fmt.Sprintf("invalid wildcard name in %q", seg)
			}
		}
	}
	return ""
}

// PrepareNode registers the route's receiver struct before resolution.
func (h *HTTP) PrepareNode(n any, g *gen.Gen) {
	nd := n.(*node)
	prepareReceiver(g, nd.recv, nd.fset)
	BindHTTPServer(g)
}
