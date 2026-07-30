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

func TestBindingNamesAndMissingBinding(t *testing.T) {
	g := New()
	pkg := types.NewPackage("app/store", "store")
	obj := types.NewTypeName(token.NoPos, pkg, "DB", nil)
	named := types.NewNamed(obj, types.NewStruct(nil, nil), nil)
	ptr := types.NewPointer(named)

	if names := g.BindingNames(ptr); len(names) != 0 {
		t.Fatalf("BindingNames on unbound type = %v", names)
	}
	if _, _, ok := g.MissingBinding(ptr, ""); ok {
		t.Fatal("unnamed miss with no bindings must leave the caller's text")
	}
	msg, help, ok := g.MissingBinding(ptr, "replica")
	if !ok || msg != `no provider for *store.DB named "replica"` {
		t.Fatalf("named miss with no bindings: %q, %v", msg, ok)
	}
	if help != "add a //fabrik:provider name=replica returning *store.DB" {
		t.Fatalf("named miss help = %q", help)
	}

	g.SetDirective("provider")
	g.BindLazy(ptr, "audit", func() (string, diag.Diagnostics) { return "a", nil })
	g.Bind(ptr, "", "storeDB")

	if names := g.BindingNames(ptr); len(names) != 2 || names[0] != "" || names[1] != "audit" {
		t.Fatalf("BindingNames = %v", names)
	}
	_, help, ok = g.MissingBinding(ptr, "replica")
	if !ok || help != "known names: (unnamed), audit" {
		t.Fatalf("named miss help with candidates = %q, %v", help, ok)
	}

	if _, _, ok := g.MissingBinding(ptr, ""); ok {
		t.Fatal("unnamed miss with an unnamed binding present must leave the caller's text")
	}

	g2 := New()
	g2.SetDirective("provider")
	g2.BindLazy(ptr, "warm", func() (string, diag.Diagnostics) { return "w", nil })
	msg, help, ok = g2.MissingBinding(ptr, "")
	if !ok || msg != "type *store.DB has only named providers" {
		t.Fatalf("named-only miss msg = %q, %v", msg, ok)
	}
	if help != "known names: warm; select one with //fabrik:inject" {
		t.Fatalf("named-only miss help = %q", help)
	}
}

func TestBindingNamesPathBinding(t *testing.T) {
	g := New()
	pkg := types.NewPackage("app/store", "store")
	obj := types.NewTypeName(token.NoPos, pkg, "DB", nil)
	named := types.NewNamed(obj, types.NewStruct(nil, nil), nil)
	ptr := types.NewPointer(named)

	g.BindLazyPath("*app/store.DB", func() (string, diag.Diagnostics) { return "db", nil })
	if names := g.BindingNames(ptr); len(names) != 1 || names[0] != "" {
		t.Fatalf("BindingNames with path binding = %v", names)
	}
	_, help, ok := g.MissingBinding(ptr, "backup")
	if !ok || help != "known names: (unnamed)" {
		t.Fatalf("path-bound named miss help = %q, %v", help, ok)
	}
}
