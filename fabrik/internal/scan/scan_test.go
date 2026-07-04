package scan_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/fabrik/internal/diag"
	"github.com/gofabrik/fabrik/fabrik/internal/scan"
)

func scanProject(t *testing.T, files map[string]string) (*scan.Project, diag.Diagnostics) {
	t.Helper()
	dir := t.TempDir()
	if _, ok := files["go.mod"]; !ok {
		files["go.mod"] = "module example\n\ngo 1.26\n"
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	project, diags, err := scan.Scan(dir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return project, diags
}

func findPkg(t *testing.T, p *scan.Project, name string) *scan.Package {
	t.Helper()
	for _, pkg := range p.Packages {
		if pkg.Name == name {
			return pkg
		}
	}
	t.Fatalf("package %q not found", name)
	return nil
}

func hasDiag(diags diag.Diagnostics, msg string, sev diag.Severity) bool {
	for _, d := range diags {
		if d.Severity == sev && strings.Contains(d.Message, msg) {
			return true
		}
	}
	return false
}

func TestScanCollectsDirectives(t *testing.T) {
	project, diags := scanProject(t, map[string]string{
		"web/web.go": `package web

import "net/http"

type Handlers struct{ Greeter *Greeter }

//fabrik:http GET /
func (h *Handlers) Index(w http.ResponseWriter, r *http.Request) {}

type Greeter struct{}

//fabrik:provider
func NewGreeter() *Greeter { return &Greeter{} }
`,
	})
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}

	web := findPkg(t, project, "web")
	if got := web.ImportPath; got != "example/web" {
		t.Errorf("import path = %q, want example/web", got)
	}
	if len(web.Routes) != 1 {
		t.Fatalf("routes = %d, want 1", len(web.Routes))
	}
	if r := web.Routes[0]; r.Method != "GET" || r.Path != "/" || r.Func != "Index" || r.Receiver != "*Handlers" {
		t.Errorf("route = %+v", r)
	}
	if len(web.Providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(web.Providers))
	}
	if p := web.Providers[0]; p.Func != "NewGreeter" || p.Returns != "*Greeter" {
		t.Errorf("provider = %+v", p)
	}
	if _, ok := web.Structs["Handlers"]; !ok {
		t.Errorf("Handlers struct not recorded")
	}
}

func TestScanDiagnostics(t *testing.T) {
	tests := []struct {
		name string
		src  string
		msg  string
		sev  diag.Severity
	}{
		{"unknown method", "//fabrik:http GTE /\nfunc F() {}", `unknown HTTP method "GTE"`, diag.SevError},
		{"missing path", "//fabrik:http GET\nfunc F() {}", "requires a PATH", diag.SevError},
		{"missing method and path", "//fabrik:http\nfunc F() {}", "requires METHOD and PATH", diag.SevError},
		{"path not rooted", "//fabrik:http GET x\nfunc F() {}", "must start with", diag.SevError},
		{"extra arg", "//fabrik:http GET / extra\nfunc F() {}", "unexpected argument", diag.SevError},
		{"unknown directive", "//fabrik:htpt GET /\nfunc F() {}", `unknown directive "fabrik:htpt"`, diag.SevError},
		{"provider takes args", "//fabrik:provider x\nfunc F() *T { return nil }\n\ntype T struct{}", "takes no arguments", diag.SevError},
		{"provider no return", "//fabrik:provider\nfunc F() {}", "requires a return value", diag.SevError},
		{"typo prefix", "//farbik:http GET /\nfunc F() {}", "did you mean", diag.SevWarning},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, diags := scanProject(t, map[string]string{
				"pkg/pkg.go": "package pkg\n\n" + tt.src + "\n",
			})
			if !hasDiag(diags, tt.msg, tt.sev) {
				t.Fatalf("want diagnostic containing %q (severity %v), got: %v", tt.msg, tt.sev, diags)
			}
		})
	}
}

func TestScanSkipsNestedModule(t *testing.T) {
	_, diags := scanProject(t, map[string]string{
		"sub/go.mod": "module sub\n\ngo 1.26\n",
		"sub/bad.go": "package sub\n\n//fabrik:htpt GET /\nfunc F() {}\n",
	})
	if len(diags) != 0 {
		t.Fatalf("nested module should be skipped, got: %v", diags)
	}
}
