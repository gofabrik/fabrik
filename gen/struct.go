package gen

import (
	"fmt"
	"go/token"
	"go/types"

	"github.com/gofabrik/fabrik/diag"
)

// RegisterStruct lazily registers construction for a pointer-to-named struct.
// Existing bindings win.
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

// EnsureStruct returns the wired expression for a handler struct.
func EnsureStruct(g *Gen, fset *token.FileSet, t types.Type) (string, diag.Diagnostics) {
	RegisterStruct(g, fset, t)
	expr, ds, ok := g.Instance(types.Unalias(t), "")
	if !ok {
		return "nil", ds
	}
	return expr, ds
}

// buildStruct emits one injected expression per exported field.
func buildStruct(g *Gen, fset *token.FileSet, named *types.Named) (string, diag.Diagnostics) {
	st := named.Underlying().(*types.Struct)

	var ds diag.Diagnostics
	var fields []Field
	for i := 0; i < st.NumFields(); i++ {
		f := st.Field(i)
		owner := g.TypeExpr(named)
		if !f.Exported() {
			// Private state stays zero-valued unless a binding makes injection likely.
			if g.HasBinding(f.Type(), "") {
				ds.Error(fset.Position(f.Pos()),
					fmt.Sprintf("field %s of %s is unexported but its type has a provider", f.Name(), owner),
					"fabrik wires from package main; export the field")
			}
			continue
		}
		expr, eds, ok := g.Instance(f.Type(), "")
		if !ok {
			if len(eds) == 0 {
				help, hinted := g.MissingHint(f.Type())
				if !hinted {
					help = fmt.Sprintf("add a //fabrik:provider returning %s", g.TypeExpr(f.Type()))
				}
				ds.Error(fset.Position(f.Pos()),
					fmt.Sprintf("no provider for %s (field %s of %s)", g.TypeExpr(f.Type()), f.Name(), owner),
					help)
			}
			expr = "nil"
		}
		for i := range eds {
			if !eds[i].Pos.IsValid() {
				eds[i].Pos = fset.Position(f.Pos())
			}
		}
		ds = append(ds, eds...)
		fields = append(fields, Field{Name: f.Name(), Expr: expr})
	}

	v := g.Var(named.Obj().Pkg().Name() + named.Obj().Name())
	g.Node(&StructLit{
		Base:   Base{Phase: PhaseWire},
		Var:    v,
		Type:   g.TypeExpr(named),
		Fields: fields,
	})
	// Bind the value form for value receivers when no provider owns it.
	if !g.HasBinding(named, "") {
		g.Bind(named, "", "*"+v)
	}
	return v, ds
}

// namedStructOf unwraps a pointer-to-named-struct type.
func namedStructOf(ptr types.Type) *types.Named {
	p := ptr.(*types.Pointer)
	return types.Unalias(p.Elem()).(*types.Named)
}
