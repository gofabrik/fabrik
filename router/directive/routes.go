package directive

import (
	"fmt"
	"go/token"
	"go/types"
	"net/http"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

const httpserverPath = "github.com/gofabrik/fabrik/httpserver"

// BindHTTPServer lazily binds an HTTP server for the router when the app imports httpserver.
func BindHTTPServer(g *gen.Gen) {
	st, ok := g.LookupType(httpserverPath, "Server")
	if !ok {
		return
	}
	ptr := types.NewPointer(st)
	if g.HasBinding(ptr, "") {
		return
	}
	g.BindLazy(ptr, "", func() (string, diag.Diagnostics) {
		var ds diag.Diagnostics
		r := g.Singleton(routerPath, "r", g.Import(routerPath)+".New()")
		srvExpr := "nil"
		if t, ok := g.LookupType("net/http", "Server"); ok {
			if e, sds, ok := g.Instance(types.NewPointer(t), ""); ok {
				ds = append(ds, sds...)
				srvExpr = e
			}
		}
		return g.Import(httpserverPath) + ".New(" + r + ", " + srvExpr + ")", ds
	})
}

// routeTable tracks registrations and delegates conflicts to http.ServeMux.
type routeTable struct {
	seen    map[string]token.Position
	order   []string // declaration order for deterministic conflict diagnostics
	scratch *http.ServeMux
}

// NewRouteTable returns the route table for one run.
func NewRouteTable() *routeTable { //nolint:revive // intentionally opaque table, constructed cross-module via :=
	return &routeTable{seen: map[string]token.Position{}, scratch: http.NewServeMux()}
}

func (rt *routeTable) add(key string, pos token.Position) (diag.Diagnostic, bool) {
	if first, dup := rt.seen[key]; dup {
		return diag.Diagnostic{
			Severity: diag.SevError,
			Pos:      pos,
			Message:  fmt.Sprintf("duplicate route %s", key),
			Help:     fmt.Sprintf("first declared at %s", first),
		}, false
	}
	if !registers(rt.scratch, key) {
		return rt.conflictDiag(key, pos), false
	}
	rt.seen[key] = pos
	rt.order = append(rt.order, key)
	return diag.Diagnostic{}, true
}

func registers(mux *http.ServeMux, pattern string) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	mux.Handle(pattern, http.NotFoundHandler())
	return true
}

// conflictDiag names the earliest conflicting pattern using ServeMux probes.
func (rt *routeTable) conflictDiag(key string, pos token.Position) diag.Diagnostic {
	for _, earlier := range rt.order {
		probe := http.NewServeMux()
		if registers(probe, earlier) && !registers(probe, key) {
			return diag.Diagnostic{
				Severity: diag.SevError,
				Pos:      pos,
				Message:  fmt.Sprintf("route %s conflicts with %s", key, earlier),
				Help: fmt.Sprintf("declared at %s; the patterns match the same requests and neither is more specific",
					rt.seen[earlier]),
			}
		}
	}
	// Keep a generic diagnostic if ServeMux conflict behavior changes.
	return diag.Diagnostic{
		Severity: diag.SevError,
		Pos:      pos,
		Message:  fmt.Sprintf("route %s conflicts with an earlier route", key),
		Help:     "the patterns match the same requests and neither is more specific",
	}
}

func effectiveRoute(groups *Group, recvObj *types.TypeName, path string, own []mwRef) (string, []mwRef) {
	var refs []mwRef
	if recvObj != nil {
		if info := groups.lookup(recvObj); info != nil {
			path = joinPattern(info.prefix, path)
			refs = append(refs, info.refs...)
		}
	}
	return path, append(refs, own...)
}

// handlerExpr returns the expression for a handler: pkg.Func for a plain
// function, or instance.Method through the wired receiver struct.
func handlerExpr(g *gen.Gen, recv types.Type, pkg *types.Package, fn string, fset *token.FileSet) (string, diag.Diagnostics) {
	if recv == nil {
		return g.ImportPkg(pkg) + "." + fn, nil
	}
	inst, ds := gen.EnsureStruct(g, fset, receiverPtr(recv))
	return inst + "." + fn, ds
}

func receiverPtr(recv types.Type) types.Type {
	t := types.Unalias(recv)
	if _, ok := t.(*types.Pointer); !ok {
		t = types.NewPointer(t)
	}
	return t
}

// prepareReceiver registers receiver bindings before dependency resolution.
func prepareReceiver(g *gen.Gen, recv types.Type, fset *token.FileSet) {
	if recv == nil {
		return
	}
	t := receiverPtr(recv)
	gen.RegisterStruct(g, fset, t)
	registerRouterFieldBinding(g, t)
}

func registerRouterFieldBinding(g *gen.Gen, t types.Type) {
	n := namedOf(t)
	if n == nil {
		return
	}
	st, ok := n.Underlying().(*types.Struct)
	if !ok {
		return
	}
	for i := 0; i < st.NumFields(); i++ {
		ft := st.Field(i).Type()
		if types.TypeString(types.Unalias(ft), nil) != "*"+routerPath+".Router" {
			continue
		}
		if !g.HasBinding(ft, "") {
			g.BindLazy(ft, "", func() (string, diag.Diagnostics) {
				return g.Singleton(routerPath, "r", g.Import(routerPath)+".New()"), nil
			})
		}
	}
}
