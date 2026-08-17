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
	byName  map[string]*mwNode
	host    *Host
	globals []*mwNode
	decls   []*mwNode

	ordOnce bool
	ord     orderResult
	ordDs   diag.Diagnostics
}

// NewMiddleware returns a Middleware directive for one run.
func NewMiddleware() *Middleware { return &Middleware{byName: map[string]*mwNode{}} }

func (*Middleware) Name() string { return "http:middleware" }

func (*Middleware) Meta() gen.Meta {
	return gen.Meta{
		Synopsis: "Middleware: direct or constructor form, global=true or referenced by name",
		Doc: "**`//fabrik:http:middleware [name=NAME] [global=true] [requires=...] [after=...] [before=...]`**\n\n" +
			"Direct form: `func(next http.Handler) http.Handler`, referenced in place. " +
			"Constructor form: binding-resolved parameters returning " +
			"`func(http.Handler) http.Handler` or `router.Middleware`, optionally with " +
			"a trailing error; it is built once before route registration. " +
			"`global=true` attaches the middleware to every route, including 404/405; " +
			"every global runs before any route middleware. `name=` is identity: routes " +
			"and groups opt in through their `middleware=` chain, and ordering options " +
			"reference names. A declaration with neither does nothing.\n\n" +
			"Ordering the global stack: `requires=x` is hard (x must exist and run " +
			"earlier; on route middleware it instead requires x to be global or listed " +
			"earlier in the chain); `after=x`/`before=x` order softly when x is a " +
			"global and stay silent when nothing declares x; `before=*`/`after=*` " +
			"place the middleware outermost/innermost. Unconstrained globals keep " +
			"declaration order (file, then line). Contradictory constraints are " +
			"generation errors.\n\n" +
			"```go\n//fabrik:http:middleware name=auth\nfunc RequireAuth(next http.Handler) http.Handler { ... }\n\n//fabrik:http:middleware name=session global=true\nfunc SessionMiddleware(m *session.Manager) func(http.Handler) http.Handler {\n\treturn m.Middleware\n}\n```",
		Example: "//fabrik:http:middleware global=true",
		Tier:    gen.TierBind,
		Attrs: []gen.AttrSpec{
			{Key: "name", Kind: gen.KindFreeform},
			{Key: "global", Kind: gen.KindFreeform, Values: []string{"true", "false"}},
			{Key: "requires", Kind: gen.KindMiddlewareRef},
			{Key: "after", Kind: gen.KindMiddlewareRef},
			{Key: "before", Kind: gen.KindMiddlewareRef},
		},
	}
}

type mwNode struct {
	pos    token.Position
	name   string // "" means unreferencable; identity only
	global bool   // global=true: runs on every route

	requires  []mwRef // hard: must exist and run earlier
	after     []mwRef // soft: order after, if the target is global
	before    []mwRef // soft: order before, if the target is global
	afterAll  bool    // after=*: inner band
	beforeAll bool    // before=*: outer band

	fn   string
	obj  types.Object
	pkg  *types.Package
	used bool // referenced by at least one middleware= chain

	ctor      bool // constructor form, built once per scope
	errResult bool
	result    types.Type
	params    []ctorParam
}

// ctorParam is one binding-resolved constructor parameter.
type ctorParam struct {
	ident string
	t     types.Type
	pos   token.Position
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
	if gl, ok := args.Attr["global"]; ok {
		switch gl.Text {
		case "true":
			nd.global = true
		case "false":
		default:
			ds.Error(a.ArgPos(gl.Col), fmt.Sprintf("invalid global value %q", gl.Text),
				"global=true or global=false")
		}
	}
	nd.requires, _, ds = parseOrderRefs(a, args, "requires", false, ds)
	nd.after, nd.afterAll, ds = parseOrderRefs(a, args, "after", true, ds)
	nd.before, nd.beforeAll, ds = parseOrderRefs(a, args, "before", true, ds)
	if nd.beforeAll && nd.afterAll {
		ds.Error(a.Pos, "before=* and after=* on one declaration contradict each other",
			"a middleware cannot be both outermost and innermost; keep one")
	}
	if !nd.global && (len(nd.after) > 0 || len(nd.before) > 0 || nd.afterAll || nd.beforeAll) {
		ds.Error(a.Pos, "after=/before= order the global stack; this declaration is not global=true",
			"route middleware order is the middleware= list; add global=true to order this in the global stack")
	}
	if ds.HasFatal() {
		return nil, ds
	}
	return nd, ds
}

// parseOrderRefs parses one comma-separated ordering attribute into
// name references, splitting out the * wildcard where allowed.
func parseOrderRefs(a gen.Annotation, args gen.Args, key string, starOK bool, ds diag.Diagnostics) ([]mwRef, bool, diag.Diagnostics) {
	arg, ok := args.Attr[key]
	if !ok {
		return nil, false, ds
	}
	var refs []mwRef
	star := false
	offset := 0
	for _, part := range strings.SplitAfter(arg.Text, ",") {
		ref := strings.TrimSuffix(part, ",")
		lead := len(ref) - len(strings.TrimLeft(ref, " \t"))
		ref = strings.TrimSpace(ref)
		pos := a.ArgPos(arg.Col + offset + lead)
		offset += len(part)

		if ref == "*" {
			if !starOK {
				ds.Error(pos, "requires=* is not a target", "requires= names a middleware; * is for after=/before= placement")
				continue
			}
			star = true
			continue
		}
		if !isIdentifier(ref) {
			ds.Error(pos, fmt.Sprintf("invalid middleware name %q in %s=", ref, key),
				"use declared names, comma-separated, or * for outermost/innermost placement")
			continue
		}
		refs = append(refs, mwRef{name: ref, pos: pos})
	}
	return refs, star, ds
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
		nd.result = sig.Results().At(0).Type()
		for v := range sig.Params().Variables() {
			if types.TypeString(types.Unalias(v.Type()), nil) == "net/http.Handler" {
				ds.Error(nd.pos, fmt.Sprintf("middleware %s is neither form: not a direct middleware (it does not return http.Handler itself), not a constructor (its http.Handler parameter cannot resolve from the binding surface)", fn.Name()),
					"direct: func(next http.Handler) http.Handler; constructor: binding-resolved parameters returning the middleware")
				return ds
			}
			nd.params = append(nd.params, ctorParam{ident: v.Name(), t: v.Type(), pos: t.Fset.Position(v.Pos())})
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
	nd.obj = fn
	nd.pkg = fn.Pkg()
	m.decls = append(m.decls, nd)
	return ds
}

func (m *Middleware) Emit(n any, g *gen.Gen) diag.Diagnostics {
	nd := n.(*mwNode)
	if !nd.global {
		// Named route middleware build on first reference.
		return nil
	}
	m.globals = append(m.globals, nd)
	if len(m.globals) > 1 {
		return nil
	}
	m.host.record(func(g *gen.Gen) diag.Diagnostics {
		// Ordering diagnostics are Validate's (they hold whether or not
		// the router is demanded); the stack is always complete, so
		// constructor diagnostics surface even when ordering fails.
		var ds diag.Diagnostics
		r := routerSingleton(g)
		for _, e := range m.ordering().stack {
			expr, eds := m.expr(g, e.nd)
			ds = append(ds, eds...)
			g.Node(&gen.Call{
				Base: gen.Base{Phase: gen.PhaseMiddleware, Origin: gen.Origin{Pos: e.nd.pos}, Label: e.label},
				Fn:   r + ".Use",
				Args: []string{expr},
			})
		}
		return ds
	})
	return nil
}

// ordering memoizes the resolved global stack so emission and
// validation agree and its diagnostics are reported exactly once.
func (m *Middleware) ordering() orderResult {
	if !m.ordOnce {
		m.ordOnce = true
		m.ord, m.ordDs = resolveOrder(m.globals, m.decls, m.byName, mwLabels(m.decls))
	}
	return m.ord
}

// expr renders one middleware reference and builds constructors once.
func (m *Middleware) expr(g *gen.Gen, nd *mwNode) (string, diag.Diagnostics) {
	if !nd.ctor {
		return g.ImportPkg(nd.pkg) + "." + nd.fn, nil
	}
	var ds diag.Diagnostics
	v := g.OnceValue(mwOnceKey(nd), func() string {
		args := make([]string, 0, len(nd.params))
		for _, pr := range nd.params {
			name := g.SelectedName(nd.obj, pr.ident)
			expr, ids, ok := g.Instance(pr.t, name)
			ds = append(ds, ids...)
			if !ok && len(ids) == 0 {
				msg, help := g.MissingBinding(pr.t, name, func() (string, string) {
					help := "declare a //fabrik:provider for it"
					if h, hinted := g.MissingHint(pr.t); hinted {
						help = h
					}
					return "no provider or binding supplies this middleware constructor parameter", help
				})
				ds.Error(pr.pos, msg, help)
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
			Type: nd.result,
		})
		return v
	})
	return v, ds
}

func mwOnceKey(nd *mwNode) string {
	return "middleware:" + nd.pkg.Path() + "." + nd.fn + "#" + nd.name
}

// Validate checks declaration-level invariants, resolves the global
// order (its contradictions are declaration-level facts, reported here
// so they fire even when no route demands the router), and warns about
// unreferenced and inert middleware.
func (m *Middleware) Validate(*gen.Gen) diag.Diagnostics {
	m.ordering()
	ds := append(diag.Diagnostics(nil), m.ordDs...)
	return append(ds, validateDecls(m.decls, m.byName, mwLabels(m.decls))...)
}

// resolve maps middleware= references to declarations and validates
// the chain: globals may not be listed (they already run), and a
// member's requires= must be satisfied by a global or an earlier
// chain entry.
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
		if nd.global {
			ds.Error(ref.pos, fmt.Sprintf("global middleware %q referenced in a middleware= chain (it would run twice)", ref.name),
				"remove it from the chain; it already runs on every route")
			continue
		}
		nd.used = true
		for _, req := range nd.requires {
			target := m.byName[req.name]
			if target == nil {
				// Unknown targets are Validate's finding.
				continue
			}
			if target.global {
				continue
			}
			earlier := false
			for _, prev := range out {
				if prev == target {
					earlier = true
					break
				}
			}
			if !earlier {
				ds.Error(ref.pos, fmt.Sprintf("middleware %q requires=%s, which is neither global nor earlier in this chain", ref.name, req.name),
					fmt.Sprintf("add %s before %s in the middleware= list", req.name, ref.name))
			}
		}
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
