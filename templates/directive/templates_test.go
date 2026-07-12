package directive

import (
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

func TestCheckEmbedPattern(t *testing.T) {
	tests := []struct {
		doc  []string
		dir  string
		want string // "" means accepted
	}{
		{doc: []string{"//fabrik:templates", "//go:embed all:templates"}, want: ""},
		{doc: []string{"//fabrik:templates", "//go:embed templates"}, want: "does not cover all:templates"},
		{doc: []string{"//fabrik:templates", "//go:embed static all:templates"}, want: ""},
		{doc: []string{"//fabrik:templates dir=views", "//go:embed all:templates"}, dir: "views", want: "does not cover all:views"},
		{doc: []string{"//fabrik:templates"}, want: "has no //go:embed"},
		{doc: []string{"//fabrik:templates", "//go:embed \"all:templates\""}, want: ""},
		{doc: []string{"//fabrik:templates", "//go:embed `all:templates`"}, want: ""},
	}
	for _, tt := range tests {
		dir := tt.dir
		if dir == "" {
			dir = "templates"
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

func TestLowerFirst(t *testing.T) {
	for in, want := range map[string]string{
		"HumanizeAge": "humanizeAge",
		"MD":          "mD",
		"upper":       "upper",
		"":            "",
	} {
		if got := gen.LowerFirst(in); got != want {
			t.Errorf("LowerFirst(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFuncNameValidation(t *testing.T) {
	for name, ok := range map[string]bool{
		"upper": true, "humanize_age": true, "md2": true,
		"bad.name": false, "2start": false, "": false, "a-b": false,
	} {
		if got := isFuncName(name); got != ok {
			t.Errorf("isFuncName(%q) = %v, want %v", name, got, ok)
		}
	}
}

func writeTree(t *testing.T, files map[string]string) *tplNode {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &tplNode{
		pos:     token.Position{Filename: filepath.Join(dir, "templates.go"), Line: 1, Column: 1},
		dir:     "templates",
		varName: "Templates",
		srcDir:  dir,
		built:   true,
	}
}

func TestValidateCollisionUsesLibraryDiscovery(t *testing.T) {
	tpl := NewTemplates()
	tpl.decls = []*tplNode{
		writeTree(t, map[string]string{
			"templates/_default/_layout.html": `{{ block "content" . }}{{ end }}`,
			"templates/_default/home.html":    `{{ define "content" }}ok{{ end }}`,
			"templates/assets/style.css":      "body {}",
		}),
		writeTree(t, map[string]string{
			"templates/web/index.html": `{{ define "content" }}web{{ end }}`,
			"templates/assets/app.css": "p {}",
		}),
	}
	if ds := tpl.Validate(nil); len(ds) != 0 {
		t.Fatalf("Validate = %v, want no diagnostics for asset directories", ds)
	}

	tpl = NewTemplates()
	tpl.decls = []*tplNode{
		writeTree(t, map[string]string{
			"templates/_default/_layout.html": `{{ block "content" . }}{{ end }}`,
			"templates/web/index.html":        `{{ define "content" }}a{{ end }}`,
		}),
		writeTree(t, map[string]string{
			"templates/web/home.html": `{{ define "content" }}b{{ end }}`,
		}),
	}
	ds := tpl.Validate(nil)
	if len(ds) != 1 || !strings.Contains(ds[0].Message, `section "web" is already provided`) {
		t.Fatalf("Validate = %v, want one web-section collision", ds)
	}
}
