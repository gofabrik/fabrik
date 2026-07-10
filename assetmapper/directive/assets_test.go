package directive

import (
	"go/token"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

func TestCheckEmbedPattern(t *testing.T) {
	tests := []struct {
		doc  []string
		dir  string
		want string // "" means accepted
	}{
		{doc: []string{"//fabrik:assets", "//go:embed all:assets"}, want: ""},
		{doc: []string{"//fabrik:assets", "//go:embed assets"}, want: "does not cover all:assets"},
		{doc: []string{"//fabrik:assets", "//go:embed static all:assets"}, want: ""},
		{doc: []string{"//fabrik:assets dir=files", "//go:embed all:assets"}, dir: "files", want: "does not cover all:files"},
		{doc: []string{"//fabrik:assets"}, want: "has no //go:embed"},
		{doc: []string{"//fabrik:assets", "//go:embed \"all:assets\""}, want: ""},
	}
	for _, tt := range tests {
		dir := tt.dir
		if dir == "" {
			dir = "assets"
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

// validateWith runs Validate over two synthetic declarations backed by
// in-memory trees keyed by srcDir.
func validateWith(trees map[string]fstest.MapFS) diag.Diagnostics {
	as := &Assets{treeFS: func(dir string) fs.FS { return trees[dir] }}
	line := 1
	for _, srcDir := range []string{"a", "b"} {
		if _, ok := trees[srcDir]; !ok {
			continue
		}
		as.decls = append(as.decls, &assetNode{
			pos:     token.Position{Filename: srcDir + "/assets.go", Line: line},
			dir:     "assets",
			varName: "Assets",
			srcDir:  srcDir,
		})
		line++
	}
	return as.Validate(nil)
}

func TestValidateUnion(t *testing.T) {
	css := &fstest.MapFile{Data: []byte("body {}")}

	if ds := validateWith(map[string]fstest.MapFS{
		"a": {"assets/style.css": css},
		"b": {"assets/app.js": {Data: []byte("export {}")}},
	}); len(ds) > 0 {
		t.Fatalf("disjoint trees: %v", ds)
	}

	ds := validateWith(map[string]fstest.MapFS{
		"a": {"assets/style.css": css},
		"b": {"assets/style.css": css},
	})
	if len(ds) == 0 || !strings.Contains(ds[0].Message, `asset "style.css" is already provided`) {
		t.Fatalf("colliding trees: %v", ds)
	}
	if !strings.Contains(ds[0].Message, "a/assets.go") || ds[0].Pos.Filename != "b/assets.go" {
		t.Fatalf("collision should point at the later declaration and name the first: %v", ds[0])
	}

	im := &fstest.MapFile{Data: []byte(`{}`)}
	ds = validateWith(map[string]fstest.MapFS{
		"a": {"assets/style.css": css, "assets/importmap.json": im},
		"b": {"assets/app.js": {Data: []byte("export {}")}, "assets/importmap.json": im},
	})
	if len(ds) == 0 || !strings.Contains(ds[0].Message, "importmap.json is already provided") {
		t.Fatalf("two importmaps: %v", ds)
	}
}

func TestValidateRunsLibraryCheck(t *testing.T) {
	ds := validateWith(map[string]fstest.MapFS{
		"a": {
			"assets/a.js": {Data: []byte(`import "./b.js";`)},
			"assets/b.js": {Data: []byte(`import "./a.js";`)},
		},
	})
	if len(ds) == 0 || !strings.Contains(ds[0].Message, "dependency cycle") {
		t.Fatalf("cycle not surfaced: %v", ds)
	}
	if ds[0].Pos.Filename != "a/assets.go" {
		t.Fatalf("library errors should anchor to the first declaration: %v", ds[0])
	}
}
