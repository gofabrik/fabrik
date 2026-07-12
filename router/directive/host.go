package directive

import (
	"fmt"
	"go/token"
	"go/types"
	"strings"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

// Host shares routing state across route-producing directives.
type Host struct {
	groups *Group
	routes *routeTable
	mw     *Middleware
}

// NewHost bundles the routing state one generation run shares.
func NewHost(groups *Group, routes *routeTable, mw *Middleware) *Host {
	return &Host{groups: groups, routes: routes, mw: mw}
}

// RouteArgs is a parsed METHOD /path [middleware=...] argument set.
type RouteArgs struct {
	Method string
	Path   string
	refs   []mwRef
}

// ParseRoute parses and validates the shared route grammar.
func (h *Host) ParseRoute(a gen.Annotation, meta gen.Meta) (RouteArgs, diag.Diagnostics) {
	args, ds := gen.ParseArgs(a, meta)
	if len(args.Pos) < 2 {
		return RouteArgs{}, ds
	}
	method, path := args.Pos[0], args.Pos[1]
	if !validMethod(method.Text) {
		ds.Error(a.ArgPos(method.Col), fmt.Sprintf("invalid HTTP method %q", method.Text),
			"any HTTP method token works: GET, POST, ..., and extensions like PURGE")
	} else if upper := strings.ToUpper(method.Text); method.Text != upper {
		// Lowercase methods register routes no real request can match.
		ds.Error(a.ArgPos(method.Col), fmt.Sprintf("HTTP method %q must be uppercase (methods are case-sensitive)", method.Text),
			"use "+upper)
	}
	if !strings.HasPrefix(path.Text, "/") {
		ds.Error(a.ArgPos(path.Col), fmt.Sprintf("route path must start with %q (got %q)", "/", path.Text),
			fmt.Sprintf("example: //fabrik:%s GET /login", a.Name))
	} else if pe := patternError(path.Text); pe != "" {
		ds.Error(a.ArgPos(path.Col), "invalid route pattern: "+pe,
			"wildcards: /{name}, /{name...} (rest of path, last), /{$} (exact match, last)")
	}
	out := RouteArgs{Method: method.Text, Path: path.Text}
	if mw, ok := args.Attr["middleware"]; ok {
		refs, rds := parseMWRefs(a, mw)
		ds = append(ds, rds...)
		out.refs = refs
	}
	return out, ds
}

// ReceiverInfo validates a handler receiver type.
func (h *Host) ReceiverInfo(recv types.Type) (*types.TypeName, bool) {
	if !isNamedStruct(recv) {
		return nil, false
	}
	return namedOf(recv).Obj(), true
}

// PrepareReceiver registers the receiver struct's bindings.
func (h *Host) PrepareReceiver(g *gen.Gen, recv types.Type, fset *token.FileSet) {
	prepareReceiver(g, recv, fset)
}

// HandlerExpr returns pkg.Fn or instance.Method.
func (h *Host) HandlerExpr(g *gen.Gen, recv types.Type, pkg *types.Package, fn string, fset *token.FileSet) (string, diag.Diagnostics) {
	return handlerExpr(g, recv, pkg, fn, fset)
}

// EmitHandle validates conflicts and emits a pattern route for a
// directive-provided handler.
func (h *Host) EmitHandle(g *gen.Gen, pattern string, pos token.Position, handler func() (string, diag.Diagnostics)) diag.Diagnostics {
	var ds diag.Diagnostics
	if d, ok := h.routes.add(pattern, pos); !ok {
		return append(ds, d)
	}
	r := g.Singleton(routerPath, "r", g.Import(routerPath)+".New()")
	hexpr, hds := handler()
	ds = append(ds, hds...)
	g.Node(&gen.Route{
		Base:    gen.Base{Phase: gen.PhaseRegister, Origin: gen.Origin{Pos: pos}},
		Router:  r,
		Kind:    gen.RouteHandle,
		Pattern: pattern,
		Handler: hexpr,
	})
	return ds
}

// EmitRoute applies groups, resolves middleware, validates conflicts, and emits a method route.
func (h *Host) EmitRoute(g *gen.Gen, args RouteArgs, recvObj *types.TypeName, pos token.Position, handler func() (string, diag.Diagnostics)) diag.Diagnostics {
	var ds diag.Diagnostics

	pattern, refs := effectiveRoute(h.groups, recvObj, args.Path, args.refs)

	mws, mds := h.mw.resolve(refs)
	ds = append(ds, mds...)

	key := args.Method + " " + pattern
	if d, ok := h.routes.add(key, pos); !ok {
		return append(ds, d)
	}

	r := g.Singleton(routerPath, "r", g.Import(routerPath)+".New()")

	hexpr, hds := handler()
	ds = append(ds, hds...)
	chain := make([]string, 0, len(mws))
	for _, mw := range mws {
		expr, eds := h.mw.expr(g, mw)
		ds = append(ds, eds...)
		chain = append(chain, expr)
	}
	g.Node(&gen.Route{
		Base:    gen.Base{Phase: gen.PhaseRegister, Origin: gen.Origin{Pos: pos}},
		Router:  r,
		Kind:    gen.RouteMethod,
		Method:  args.Method,
		Pattern: pattern,
		Handler: hexpr,
		Chain:   chain,
	})
	return ds
}
