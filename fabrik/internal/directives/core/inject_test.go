package core

import (
	"go/token"
	"go/types"
	"testing"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

func TestInjectValidateRejectsProvidedType(t *testing.T) {
	pkg := types.NewPackage("app/store", "store")
	obj := types.NewTypeName(token.NoPos, pkg, "Deps", nil)
	named := types.NewNamed(obj, types.NewStruct(nil, nil), nil)

	g := gen.New()
	g.SetDirective("provider")
	g.BindLazy(types.NewPointer(named), "", func() (string, diag.Diagnostics) { return "storeDeps", nil })
	g.SeedInjectNames(map[types.Object]map[string]string{obj: {"DB": "replica"}})

	i := NewInject()
	i.nodes = append(i.nodes, &injectNode{selector: "DB", provName: "replica", obj: obj})

	ds := i.Validate(g)
	if !ds.HasFatal() {
		t.Fatal("expected the exclusion diagnostic")
	}
	if g.InjectPending(obj, "DB") {
		t.Fatal("excluded mapping must be marked rejected, not left pending")
	}
}
