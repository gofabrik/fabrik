package directive

import (
	"fmt"
	"go/token"
	"go/types"
	"net/http"
	"regexp"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

// routeTable tracks all patterns registered in one generation run.
type routeTable struct {
	seen    map[string]token.Position
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
	if msg := registerScratch(rt.scratch, key); msg != "" {
		return rt.conflictDiag(key, pos, msg), false
	}
	rt.seen[key] = pos
	return diag.Diagnostic{}, true
}

// registerScratch converts ServeMux registration panics to messages.
func registerScratch(mux *http.ServeMux, pattern string) (msg string) {
	defer func() {
		if p := recover(); p != nil {
			msg = fmt.Sprint(p)
		}
	}()
	mux.Handle(pattern, http.NotFoundHandler())
	return ""
}

var conflictRE = regexp.MustCompile(`conflicts with pattern "([^"]+)"`)

// conflictDiag turns a ServeMux conflict into a positioned diagnostic.
func (rt *routeTable) conflictDiag(key string, pos token.Position, panicMsg string) diag.Diagnostic {
	d := diag.Diagnostic{
		Severity: diag.SevError,
		Pos:      pos,
		Message:  fmt.Sprintf("route %s conflicts with an earlier route", key),
		Help:     "the patterns match the same requests and neither is more specific",
	}
	if m := conflictRE.FindStringSubmatch(panicMsg); m != nil {
		d.Message = fmt.Sprintf("route %s conflicts with %s", key, m[1])
		if first, ok := rt.seen[m[1]]; ok {
			d.Help = fmt.Sprintf("declared at %s; the patterns match the same requests and neither is more specific", first)
		}
	}
	return d
}

// handlerExpr returns the expression for a handler: pkg.Func for a plain
// function, or instance.Method through the wired receiver struct.
func handlerExpr(g *gen.Gen, recv types.Type, pkg *types.Package, fn string, fset *token.FileSet) (string, diag.Diagnostics) {
	if recv == nil {
		return g.ImportPkg(pkg) + "." + fn, nil
	}
	// Wire through the pointer type so the instance is a selector-friendly
	// variable; methods on value receivers are reachable through it too.
	t := types.Unalias(recv)
	if _, ok := t.(*types.Pointer); !ok {
		t = types.NewPointer(t)
	}
	bindRouterFields(g, t)
	inst, ds := gen.EnsureStruct(g, fset, t)
	return inst + "." + fn, ds
}

// bindRouterFields makes *router.Router injectable into handler structs.
func bindRouterFields(g *gen.Gen, t types.Type) {
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
		if _, _, bound := g.Instance(ft, ""); !bound {
			r := g.Singleton(routerPath, "r", g.Import(routerPath)+".New()")
			g.Bind(ft, "", r)
		}
	}
}
