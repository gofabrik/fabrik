package gen

import (
	"fmt"
	"go/token"
	"go/types"
	"strings"

	"github.com/gofabrik/fabrik/diag"
)

// RegisterStruct lazily registers the wiring for a handler struct, given a
// pointer-to-named-struct type. Registration up front (before anything
// resolves dependencies) makes struct injection independent of emission
// order. An existing binding for the type - e.g. a user provider - wins.
func RegisterStruct(g *Gen, fset *token.FileSet, t types.Type) {
	ptr := types.Unalias(t)
	if g.HasBinding(ptr, "") {
		return
	}
	named := namedStructOf(ptr)
	g.BindLazy(ptr, "", func() (string, diag.Diagnostics) {
		return buildStruct(g, fset, named)
	})
}

// EnsureStruct returns the wired expression for a handler struct, given a
// pointer-to-named-struct type, registering it first if needed.
func EnsureStruct(g *Gen, fset *token.FileSet, t types.Type) (string, diag.Diagnostics) {
	RegisterStruct(g, fset, t)
	expr, ds, ok := g.Instance(types.Unalias(t), "")
	if !ok {
		// Only a dependency cycle through the struct itself lands here; the
		// diagnostics explain it.
		return "nil", ds
	}
	return expr, ds
}

// buildStruct emits `v := &pkg.T{...}` with one injected expression per
// exported field. Embedded fields wire like named ones (their field name is
// the type name); unexported and unresolved fields produce diagnostics.
func buildStruct(g *Gen, fset *token.FileSet, named *types.Named) (string, diag.Diagnostics) {
	st := named.Underlying().(*types.Struct)

	var ds diag.Diagnostics
	var fields []string
	for i := 0; i < st.NumFields(); i++ {
		f := st.Field(i)
		owner := g.TypeExpr(named)
		if !f.Exported() {
			ds.Error(fset.Position(f.Pos()),
				fmt.Sprintf("field %s of %s is unexported", f.Name(), owner),
				"fabrik wires from package main; export the field")
			continue
		}
		expr, eds, ok := g.Instance(f.Type(), "")
		if !ok {
			if len(eds) == 0 {
				ds.Error(fset.Position(f.Pos()),
					fmt.Sprintf("no provider for %s (field %s of %s)", g.TypeExpr(f.Type()), f.Name(), owner),
					fmt.Sprintf("add a //fabrik:provider returning %s", g.TypeExpr(f.Type())))
			}
			expr = "nil"
		}
		for i := range eds {
			if !eds[i].Pos.IsValid() {
				eds[i].Pos = fset.Position(f.Pos())
			}
		}
		ds = append(ds, eds...)
		fields = append(fields, fmt.Sprintf("%s: %s,", f.Name(), expr))
	}

	v := g.Var(named.Obj().Pkg().Name() + named.Obj().Name())
	if len(fields) == 0 {
		g.Stmt(PhaseWire, "%s := &%s{}", v, g.TypeExpr(named))
	} else {
		g.Stmt(PhaseWire, "%s := &%s{\n%s\n}", v, g.TypeExpr(named), strings.Join(fields, "\n"))
	}
	// The pointer form is bound by Instance when this build returns. The
	// value form may already be bound by a value-returning provider.
	if !g.HasBinding(named, "") {
		g.Bind(named, "", "*"+v)
	}
	return v, ds
}

// namedStructOf unwraps a pointer-to-named-struct type; callers guarantee
// the shape at Check time.
func namedStructOf(ptr types.Type) *types.Named {
	p := ptr.(*types.Pointer)
	return types.Unalias(p.Elem()).(*types.Named)
}
