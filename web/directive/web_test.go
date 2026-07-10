package directive

import (
	"go/types"
	"testing"
)

// TestTypePathUnaliasesThroughPointers pins alias-robust signature checks.
func TestTypePathUnaliasesThroughPointers(t *testing.T) {
	webPkg := types.NewPackage("github.com/gofabrik/fabrik/web", "web")
	request := types.NewNamed(
		types.NewTypeName(0, webPkg, "Request", nil),
		types.NewStruct(nil, nil), nil)

	appPkg := types.NewPackage("app/web", "web")
	alias := types.NewAlias(types.NewTypeName(0, appPkg, "Req", nil), request)

	if got := typePath(types.NewPointer(alias)); got != requestPath {
		t.Fatalf("typePath(*alias) = %q, want %q", got, requestPath)
	}
	if got := typePath(types.NewPointer(request)); got != requestPath {
		t.Fatalf("typePath(*named) = %q, want %q", got, requestPath)
	}
	if got := typePath(types.Universe.Lookup("error").Type()); got != "error" {
		t.Fatalf("typePath(error) = %q", got)
	}
}
