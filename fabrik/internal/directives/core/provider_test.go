package core

import (
	"go/token"
	"go/types"
	"testing"

	"github.com/gofabrik/fabrik/gen"
)

func TestNamedProviderVariable(t *testing.T) {
	pkg := types.NewPackage("app/store", "store")
	obj := types.NewTypeName(token.NoPos, pkg, "DB", nil)
	named := types.NewNamed(obj, types.NewStruct(nil, nil), nil)
	ptr := types.NewPointer(named)

	p := NewProvider(nil)
	g := gen.New()
	nd := &node{fn: "NewReplicaDB", pkg: pkg, returns: []types.Type{ptr}, name: "replica"}
	if ds := p.Emit(nd, g); ds.HasFatal() {
		t.Fatalf("emit: %v", ds)
	}
	v, ds, ok := g.Instance(ptr, "replica")
	if !ok || ds.HasFatal() {
		t.Fatalf("instance: ok=%v diags=%v", ok, ds)
	}
	if v != "storeDBReplica" {
		t.Fatalf("variable = %q, want storeDBReplica", v)
	}
}
