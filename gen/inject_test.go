package gen

import (
	"go/token"
	"go/types"
	"testing"

	"github.com/gofabrik/fabrik/diag"
)

func TestInjectMappings(t *testing.T) {
	g := New()
	pkg := types.NewPackage("app/store", "store")
	fn := types.NewFunc(token.NoPos, pkg, "NewSvc", types.NewSignatureType(nil, nil, nil, nil, nil, false))

	g.SeedInjectNames(map[types.Object]map[string]string{
		fn: {"db": "replica", "log": "audit"},
	})

	if name, ok := g.InjectName(fn, "db"); !ok || name != "replica" {
		t.Fatalf("InjectName(db) = %q, %v", name, ok)
	}
	if _, ok := g.InjectName(fn, "missing"); ok {
		t.Fatal("InjectName(missing) reported a mapping")
	}
	if !g.InjectPending(fn, "db") || !g.InjectPending(fn, "log") {
		t.Fatal("seeded mappings must start pending")
	}

	g.ConsumeInject(fn, "db")
	if g.InjectPending(fn, "db") {
		t.Fatal("consumed mapping still pending")
	}
	if name, ok := g.InjectName(fn, "db"); !ok || name != "replica" {
		t.Fatalf("InjectName after consume = %q, %v", name, ok)
	}

	g.RejectInject(fn, "log")
	if g.InjectPending(fn, "log") {
		t.Fatal("rejected mapping still pending")
	}
	if g.InjectPending(fn, "missing") {
		t.Fatal("unknown mapping reported pending")
	}
}

func TestBindingOwner(t *testing.T) {
	g := New()
	pkg := types.NewPackage("app/store", "store")
	obj := types.NewTypeName(token.NoPos, pkg, "DB", nil)
	named := types.NewNamed(obj, types.NewStruct(nil, nil), nil)
	ptr := types.NewPointer(named)

	g.SetDirective("provider")
	g.BindLazy(ptr, "", func() (string, diag.Diagnostics) { return "storeDB", nil })

	if owner, ok := g.BindingOwner(ptr, ""); !ok || owner != "provider" {
		t.Fatalf("BindingOwner = %q, %v", owner, ok)
	}
	if owner, ok := g.BindingOwner(types.NewPointer(named), ""); !ok || owner != "provider" {
		t.Fatalf("BindingOwner(fresh pointer) = %q, %v", owner, ok)
	}
	if _, ok := g.BindingOwner(named, ""); ok {
		t.Fatal("BindingOwner reported an unbound type")
	}
	if _, ok := g.BindingOwner(ptr, "replica"); ok {
		t.Fatal("BindingOwner reported an unbound name")
	}
}
