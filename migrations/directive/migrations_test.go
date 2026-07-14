package directive

import (
	"go/token"
	"go/types"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

func TestValidStream(t *testing.T) {
	for name, ok := range map[string]bool{
		"auth": true, "internal/billing": true, "a_b-c": true,
		"": false, "/auth": false, "auth/": false, "a//b": false,
		".": false, "..": false, "a/../b": false,
	} {
		if err := validStream(name); (err == nil) != ok {
			t.Errorf("validStream(%q) = %v, want ok=%v", name, err, ok)
		}
	}
}

func TestCheckEmbedPattern(t *testing.T) {
	tests := []struct {
		doc  []string
		dir  string
		want string // "" means accepted
	}{
		{doc: []string{"//fabrik:migrations", "//go:embed all:migrations"}, want: ""},
		{doc: []string{"//fabrik:migrations", "//go:embed migrations"}, want: "does not cover all:migrations"},
		{doc: []string{"//fabrik:migrations dir=files", "//go:embed all:migrations"}, dir: "files", want: "does not cover all:files"},
		{doc: []string{"//fabrik:migrations"}, want: "has no //go:embed"},
	}
	for _, tt := range tests {
		dir := tt.dir
		if dir == "" {
			dir = "migrations"
		}
		var ds diag.Diagnostics
		checkEmbedPattern(gen.Annotation{Doc: tt.doc}, dir, &ds)
		if tt.want == "" && len(ds) > 0 {
			t.Errorf("doc %v rejected: %v", tt.doc, ds)
		}
		if tt.want != "" && (len(ds) == 0 || !strings.Contains(ds[0].Message, tt.want)) {
			t.Errorf("doc %v = %v, want %q", tt.doc, ds, tt.want)
		}
	}
}

func TestResolveStreams(t *testing.T) {
	g := gen.New()
	g.SetModule("app")
	mg := NewMigrations()
	mg.decls = []*migNode{
		{pkg: types.NewPackage("app/shared", "shared"), pos: token.Position{Filename: "shared/m.go", Line: 1}},
		{pkg: types.NewPackage("app/internal/billing", "billing"), pos: token.Position{Filename: "b/m.go", Line: 1}},
		{stream: "custom", pkg: types.NewPackage("app/x", "x"), pos: token.Position{Filename: "x/m.go", Line: 1}},
	}
	if ds := mg.resolveStreams(g); len(ds) > 0 {
		t.Fatalf("resolveStreams: %v", ds)
	}
	got := []string{mg.decls[0].resolved, mg.decls[1].resolved, mg.decls[2].resolved}
	want := []string{"shared", "internal/billing", "custom"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("resolved[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// A package at the module root cannot derive a name.
	root := NewMigrations()
	root.decls = []*migNode{{pkg: types.NewPackage("app", "app"), pos: token.Position{Filename: "m.go", Line: 1}}}
	ds := root.resolveStreams(g)
	if len(ds) == 0 || !strings.Contains(ds[0].Message, "module root") {
		t.Fatalf("root package: ds = %v, want module-root error", ds)
	}
}
