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
	groups *Group
	routes *routeTable
}

// NewHTTP returns an HTTP directive for one run.
func NewHTTP(groups *Group, routes *routeTable) *HTTP {
	return &HTTP{groups: groups, routes: routes}
}

func (*HTTP) Name() string { return "http" }

func (*HTTP) Meta() gen.Meta {
	return gen.Meta{
		Synopsis: "HTTP route: METHOD /path [middleware=A,B]",
		Doc: "**`//fabrik:http METHOD /path [middleware=Name,pkg.Name]`**\n\n" +
			"Registers a standard `net/http` handler on the fabrik router. " +
			"METHOD is any HTTP method token, including extensions like PURGE. " +
			"Handler signature: `func(w http.ResponseWriter, r *http.Request)`, " +
			"on a plain function or a method of a wired struct. " +
			"`middleware=` wraps this route in a comma-separated chain, " +
			"outermost first; a bare name resolves in the handler's package.\n\n" +
			"```go\n//fabrik:http GET /login\n//fabrik:http POST /account middleware=shared.RequireAuth\n```",
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

// mwRef is one unresolved middleware reference from a middleware= chain.
type mwRef struct {
	pkg  string // "" resolves in the handler's package
	name string
	pos  token.Position
}

func (r mwRef) String() string {
	if r.pkg == "" {
		return r.name
	}
	return r.pkg + "." + r.name
}

type node struct {
	method, path string
	pos          token.Position
	refs         []mwRef

	fn      string
	pkg     *types.Package
	recv    types.Type      // nil for a plain function handler
	recvObj *types.TypeName // the receiver's type name, for group lookup
	mws     []*types.Func
	fset    *token.FileSet
}

func (h *HTTP) Parse(a gen.Annotation) (any, diag.Diagnostics) {
	args, ds := gen.ParseArgs(a, h.Meta())
	if len(args.Pos) < 2 {
		return nil, ds
	}
	method, path := args.Pos[0], args.Pos[1]
	if !validMethod(method.Text) {
		ds.Error(a.ArgPos(method.Col), fmt.Sprintf("invalid HTTP method %q", method.Text),
			"any HTTP method token works: GET, POST, ..., and extensions like PURGE")
	} else if upper := strings.ToUpper(method.Text); method.Text != upper {
		// Routing is case-sensitive: a lowercase method registers a route no
		// real request would ever match.
		ds.Error(a.ArgPos(method.Col), fmt.Sprintf("HTTP method %q must be uppercase (methods are case-sensitive)", method.Text),
			"use "+upper)
	}
	if !strings.HasPrefix(path.Text, "/") {
		ds.Error(a.ArgPos(path.Col), fmt.Sprintf("route path must start with %q (got %q)", "/", path.Text),
			"example: //fabrik:http GET /login")
	} else if pe := patternError(path.Text); pe != "" {
		ds.Error(a.ArgPos(path.Col), "invalid route pattern: "+pe,
			"wildcards: /{name}, /{name...} (rest of path, last), /{$} (exact match, last)")
	}
	nd := &node{method: method.Text, path: path.Text, pos: a.Pos}
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

// parseMWRefs splits middleware= into positioned Name or pkg.Name references.
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

		pkg, name, qualified := strings.Cut(ref, ".")
		if !qualified {
			pkg, name = "", ref
		}
		if name == "" || !isIdentifier(name) || (qualified && !isIdentifier(pkg)) {
			ds.Error(pos, fmt.Sprintf("invalid middleware reference %q", ref),
				"use Name or pkg.Name, comma-separated")
			continue
		}
		refs = append(refs, mwRef{pkg: pkg, name: name, pos: pos})
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
		nd.recvObj = namedOf(recv.Type()).Obj()
	}

	mws, mds := resolveMWRefs(t, fn.Pkg(), nd.refs)
	ds = append(ds, mds...)
	nd.mws = mws

	nd.fn = fn.Name()
	nd.pkg = fn.Pkg()
	nd.fset = t.Fset
	return ds
}

// resolveMWRefs resolves and validates middleware references.
func resolveMWRefs(t gen.Typed, own *types.Package, refs []mwRef) ([]*types.Func, diag.Diagnostics) {
	var out []*types.Func
	var ds diag.Diagnostics
	for _, ref := range refs {
		var obj types.Object
		switch {
		case ref.pkg == "":
			obj = own.Scope().Lookup(ref.name)
		case t.Lookup != nil:
			var ambiguous []string
			obj, ambiguous = t.Lookup(ref.pkg, ref.name)
			if len(ambiguous) > 0 {
				ds.Error(ref.pos, fmt.Sprintf("package name %q in middleware %s is ambiguous", ref.pkg, ref),
					"matches "+strings.Join(ambiguous, " and ")+"; rename one of the packages")
				continue
			}
		}
		mf, isFunc := obj.(*types.Func)
		if !isFunc {
			ds.Error(ref.pos, fmt.Sprintf("cannot resolve middleware %s", ref),
				"expected a package-level func(next http.Handler) http.Handler")
			continue
		}
		if isGenericFunc(mf) {
			ds.Error(ref.pos, fmt.Sprintf("middleware %s cannot be generic (generated code cannot instantiate type parameters)", ref),
				"declare a concrete middleware")
			continue
		}
		if !isMiddlewareSignature(mf.Signature()) {
			ds.Error(ref.pos, fmt.Sprintf("middleware %s has the wrong signature", ref),
				"want func(next http.Handler) http.Handler")
			continue
		}
		out = append(out, mf)
	}
	return out, ds
}

func (h *HTTP) Emit(n any, g *gen.Gen) diag.Diagnostics {
	nd := n.(*node)
	var ds diag.Diagnostics

	// Apply groups before checking duplicate effective patterns.
	pattern, mws := effectiveRoute(h.groups, nd.recvObj, nd.path, nd.mws)

	key := nd.method + " " + pattern
	if d, ok := h.routes.add(key, nd.pos); !ok {
		return append(ds, d)
	}

	r := g.Singleton(routerPath, "r", g.Import(routerPath)+".New()")

	handler, hds := handlerExpr(g, nd.recv, nd.pkg, nd.fn, nd.fset)
	ds = append(ds, hds...)
	var chain strings.Builder
	for _, mf := range mws {
		chain.WriteString(", " + g.ImportPkg(mf.Pkg()) + "." + mf.Name())
	}
	g.Stmt(gen.PhaseRegister, "%s.Method(%q, %q, %s%s)", r, nd.method, pattern, handler, chain.String())
	return ds
}

// validMethod reports whether m is a non-empty HTTP method token.
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

// isHandlerSignature reports whether sig is func(http.ResponseWriter, *http.Request).
func isHandlerSignature(sig *types.Signature) bool {
	p := sig.Params()
	return p.Len() == 2 && sig.Results().Len() == 0 && !sig.Variadic() &&
		types.TypeString(p.At(0).Type(), nil) == "net/http.ResponseWriter" &&
		types.TypeString(p.At(1).Type(), nil) == "*net/http.Request"
}

// isGenericFunc reports whether fn has type parameters of its own or via
// its receiver - generated code cannot instantiate either.
func isGenericFunc(fn *types.Func) bool {
	sig := fn.Signature()
	return sig.TypeParams().Len() > 0 || sig.RecvTypeParams().Len() > 0
}

// isNamedStruct reports whether t is a named struct or a pointer to one.
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
		open, close := strings.Count(seg, "{"), strings.Count(seg, "}")
		if open == 0 && close == 0 {
			continue
		}
		if open != 1 || close != 1 || !strings.HasPrefix(seg, "{") || !strings.HasSuffix(seg, "}") {
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
}
