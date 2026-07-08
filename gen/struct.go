package gen

import (
	"fmt"
	"go/token"
	"go/types"
	"strings"

	"github.com/gofabrik/fabrik/diag"
)

// EnsureStruct wires a named struct from exported fields.
func EnsureStruct(g *Gen, fset *token.FileSet, t types.Type) (string, diag.Diagnostics) {
	if expr, ds, ok := g.Instance(t, ""); ok {
		return expr, ds
	}

	elem := types.Unalias(t)
	deref := elem
	if p, ok := elem.(*types.Pointer); ok {
		deref = types.Unalias(p.Elem())
	}
	named := deref.(*types.Named)
	st := named.Underlying().(*types.Struct)

	var ds diag.Diagnostics
	var fields []string
	for i := 0; i < st.NumFields(); i++ {
		f := st.Field(i)
		if f.Embedded() {
			continue
		}
		owner := g.TypeExpr(named)
		if !f.Exported() {
			ds.Error(fset.Position(f.Pos()),
				fmt.Sprintf("field %s of %s is unexported", f.Name(), owner),
				"fabrik wires from package main; export the field")
			continue
		}
		expr, eds, ok := g.Instance(f.Type(), "")
		ds = append(ds, eds...)
		if !ok {
			ds.Error(fset.Position(f.Pos()),
				fmt.Sprintf("no provider for %s (field %s of %s)", g.TypeExpr(f.Type()), f.Name(), owner),
				fmt.Sprintf("add a //fabrik:provider returning %s", g.TypeExpr(f.Type())))
			expr = "nil"
		}
		fields = append(fields, fmt.Sprintf("%s: %s,", f.Name(), expr))
	}

	v := g.Var(named.Obj().Pkg().Name() + named.Obj().Name())
	if len(fields) == 0 {
		g.Stmt(PhaseWire, "%s := &%s{}", v, g.TypeExpr(named))
	} else {
		g.Stmt(PhaseWire, "%s := &%s{\n%s\n}", v, g.TypeExpr(named), strings.Join(fields, "\n"))
	}
	g.Bind(types.NewPointer(named), "", v)
	g.Bind(named, "", "*"+v)

	if _, isPtr := elem.(*types.Pointer); isPtr {
		return v, ds
	}
	return "*" + v, ds
}
