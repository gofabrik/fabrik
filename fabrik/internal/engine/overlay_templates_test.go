package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWireTemplateOverlay(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	templateDir, err := filepath.Abs("../../../templates")
	if err != nil {
		t.Fatal(err)
	}
	write := func(rel, content string) {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module app\n\ngo 1.26\n\nrequire github.com/gofabrik/fabrik/templates v0.0.0\n\nreplace github.com/gofabrik/fabrik/templates => "+templateDir+"\n")
	write("main.go", "package main\n\nfunc main() { _ = run }\n")
	write("web/templates.go", "package web\n\nimport \"embed\"\n\n//fabrik:templates\n//go:embed all:templates\nvar Templates embed.FS\n")
	write("web/templates/_default/_layout.html", `{{ block "content" . }}{{ end }}`)
	write("web/templates/_default/home.html", `{{ define "content" }}ok{{ end }}`)
	write("web/web.go", `package web

import (
	"net/http"

	"github.com/gofabrik/fabrik/templates"
)

type Handlers struct {
	Templates *templates.Set
}

//fabrik:http GET /
func (h *Handlers) Home(w http.ResponseWriter, r *http.Request) {
	h.Templates.Render(w, "home", nil)
}
`)

	res, err := Wire(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Diags.HasFatal() {
		t.Fatalf("clean tree should wire: %v", res.Diags)
	}

	wantDiag := func(t *testing.T, overlay map[string][]byte, needle string) {
		t.Helper()
		res, err := Wire(dir, overlay)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range res.Diags {
			if strings.Contains(d.Message, needle) {
				return
			}
		}
		t.Fatalf("overlay template error %q not surfaced: %v", needle, res.Diags)
	}

	wantDiag(t, map[string][]byte{
		filepath.Join(dir, "web/templates/_default/home.html"): []byte(`{{ define "content" }}{{ nosuchfunc . }}{{ end }}`),
	}, "nosuchfunc")

	wantDiag(t, map[string][]byte{
		filepath.Join(dir, "web/templates/_default/extra.html"): []byte(`{{ define "content" }}{{ newfilefunc . }}{{ end }}`),
	}, "newfilefunc")

	wantDiag(t, map[string][]byte{
		filepath.Join(dir, "web/templates/drafts/page.html"): []byte(`{{ define "content" }}{{ newdirfunc . }}{{ end }}`),
	}, "newdirfunc")
}
