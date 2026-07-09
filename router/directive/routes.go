package directive

import (
	"fmt"
	"go/token"
	"go/types"
	"net/http"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

// routeTable tracks all patterns registered in one generation run. It
// delegates the conflict decision to http.ServeMux - the specificity
// rules stay the standard library's, never a second implementation.
type routeTable struct {
	seen    map[string]token.Position
	order   []string // registration order, for deterministic conflict search
	scratch *http.ServeMux
}

// NewRouteTable returns the route table for one run.
func NewRouteTable() *routeTable {
	return &routeTable{seen: map[string]token.Position{}, scratch: http.NewServeMux()}
}

// add registers a method pattern or path pattern.
func (rt *routeTable) add(key string, pos token.Position) (diag.Diagnostic, bool) {
	if first, dup := rt.seen[key]; dup {
		return diag.Diagnostic{Severity: diag.SevError, Pos: pos,
			Message: fmt.Sprintf("duplicate route %s", key),
			Help:    fmt.Sprintf("first declared at %s", first),
		}, false
	}
	if !registers(rt.scratch, key) {
		return rt.conflictDiag(key, pos), false
	}
	rt.seen[key] = pos
	rt.order = append(rt.order, key)
	return diag.Diagnostic{}, true
}

// registers reports whether the pattern registers without conflicting.
func registers(mux *http.ServeMux, pattern string) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	mux.Handle(pattern, http.NotFoundHandler())
	return true
}

// conflictDiag names the earliest registered pattern that conflicts with
// key, found by probing pairs on fresh muxes - the culprit comes from
// ServeMux's own verdict, not from parsing its panic message.
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
	// A conflict against the combined mux always has a pairwise culprit;
	// this fallback only guards a future ServeMux behavior change.
	return diag.Diagnostic{
		Severity: diag.SevError,
		Pos:      pos,
		Message:  fmt.Sprintf("route %s conflicts with an earlier route", key),
		Help:     "the patterns match the same requests and neither is more specific",
	}
}

// effectiveRoute applies the receiver group's prefix and middleware
// references; the route resolves the merged chain against declared names.
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

// receiverPtr normalizes a receiver type to its pointer form.
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

// registerRouterFieldBinding makes *router.Router injectable.
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
