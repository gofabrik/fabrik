package codegen_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/fabrik/internal/codegen"
	"github.com/gofabrik/fabrik/fabrik/internal/diag"
	"github.com/gofabrik/fabrik/fabrik/internal/scan"
)

func generate(t *testing.T, files map[string]string) (string, diag.Diagnostics) {
	t.Helper()
	dir := t.TempDir()
	if _, ok := files["go.mod"]; !ok {
		files["go.mod"] = "module example\n\ngo 1.26\n"
	}
	if _, ok := files["main.go"]; !ok {
		files["main.go"] = "package main\n\nfunc main() { _ = run() }\n"
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
	project, sdiags, err := scan.Scan(dir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	src, gdiags, err := codegen.Generate(project)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return string(src), append(sdiags, gdiags...)
}

func hasDiag(diags diag.Diagnostics, msg string) bool {
	for _, d := range diags {
		if strings.Contains(d.Message, msg) {
			return true
		}
	}
	return false
}

const greeterProject = `package web

import "net/http"

type Greeter struct{}

//fabrik:provider
func NewGreeter() *Greeter { return &Greeter{} }

func (g *Greeter) Greet(s string) string { return s }

type Handlers struct{ Greeter *Greeter }

//fabrik:http GET /
func (h *Handlers) Index(w http.ResponseWriter, r *http.Request) {}
`

func TestGenerateStructure(t *testing.T) {
	src, diags := generate(t, map[string]string{"web/web.go": greeterProject})
	if diags.HasFatal() {
		t.Fatalf("unexpected fatal diagnostics: %v", diags)
	}

	wants := []string{
		"func run() error {\n\t// Providers\n",                             // no blank line before providers
		"\twebGreeter := web.NewGreeter()\n",                               // provider constructed
		"\twebHandlers := &web.Handlers{\n\t\tGreeter: webGreeter,\n\t}\n", // struct-field injection
		"\n\t// Routes\n\tmux := http.NewServeMux()\n",                     // routes section
		"\tmux.HandleFunc(\"GET /\", webHandlers.Index)\n",                 // route registered
		"\t\"example/web\"\n",                                              // app package imported
	}
	for _, w := range wants {
		if !strings.Contains(src, w) {
			t.Errorf("generated source missing %q:\n%s", w, src)
		}
	}
	if strings.Contains(src, "---") {
		t.Errorf("generated comments should not contain ---:\n%s", src)
	}
}

func TestGeneratePrunesUnusedProvider(t *testing.T) {
	src, diags := generate(t, map[string]string{
		"web/web.go": `package web

import "net/http"

type Handlers struct{}

//fabrik:http GET /
func (h *Handlers) Index(w http.ResponseWriter, r *http.Request) {}

type Unused struct{}

//fabrik:provider
func NewUnused() *Unused { return &Unused{} }
`,
	})
	if diags.HasFatal() {
		t.Fatalf("unexpected fatal diagnostics: %v", diags)
	}
	if strings.Contains(src, "NewUnused") {
		t.Errorf("unused provider should be pruned:\n%s", src)
	}
}

func TestGenerateContextProvider(t *testing.T) {
	used := `package web

import (
	"context"
	"net/http"
)

type Store struct{}

//fabrik:provider
func NewStore(ctx context.Context) *Store { _ = ctx; return &Store{} }

type Handlers struct{ Store *Store }

//fabrik:http GET /
func (h *Handlers) Index(w http.ResponseWriter, r *http.Request) {}
`
	src, diags := generate(t, map[string]string{"web/web.go": used})
	if diags.HasFatal() {
		t.Fatalf("unexpected fatal diagnostics: %v", diags)
	}
	if !strings.Contains(src, "ctx := context.Background()") {
		t.Errorf("ctx var not emitted:\n%s", src)
	}
	if !strings.Contains(src, "web.NewStore(ctx)") {
		t.Errorf("ctx not passed to provider:\n%s", src)
	}
}

func TestGenerateContextPrunedWhenProviderUnused(t *testing.T) {
	unused := `package web

import (
	"context"
	"net/http"
)

type Store struct{}

//fabrik:provider
func NewStore(ctx context.Context) *Store { _ = ctx; return &Store{} }

type Handlers struct{}

//fabrik:http GET /
func (h *Handlers) Index(w http.ResponseWriter, r *http.Request) {}
`
	src, _ := generate(t, map[string]string{"web/web.go": unused})
	if strings.Contains(src, "context.Background()") {
		t.Errorf("ctx should not be emitted when its provider is pruned:\n%s", src)
	}
}

func TestGenerateDiagnostics(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		msg   string
	}{
		{
			"duplicate route",
			map[string]string{"web/web.go": `package web

import "net/http"

type Handlers struct{}

//fabrik:http GET /
func (h *Handlers) A(w http.ResponseWriter, r *http.Request) {}

//fabrik:http GET /
func (h *Handlers) B(w http.ResponseWriter, r *http.Request) {}
`},
			"duplicate route GET /",
		},
		{
			"missing provider",
			map[string]string{"web/web.go": `package web

import "net/http"

type Dep struct{}

type Handlers struct{ Dep *Dep }

//fabrik:http GET /
func (h *Handlers) A(w http.ResponseWriter, r *http.Request) {}
`},
			"no provider for *Dep",
		},
		{
			"duplicate provider",
			map[string]string{"web/web.go": `package web

import "net/http"

type T struct{}

//fabrik:provider
func NewA() *T { return nil }

//fabrik:provider
func NewB() *T { return nil }

type Handlers struct{ T *T }

//fabrik:http GET /
func (h *Handlers) A(w http.ResponseWriter, r *http.Request) {}
`},
			"multiple providers for type *web.T",
		},
		{
			"non-struct receiver",
			map[string]string{"web/web.go": `package web

import "net/http"

type Counter int

//fabrik:http GET /
func (c Counter) Show(w http.ResponseWriter, r *http.Request) {}
`},
			"is not a struct",
		},
		{
			"duplicate package name",
			map[string]string{
				"web/web.go": `package web

import "net/http"

type Handlers struct{}

//fabrik:http GET /
func (h *Handlers) A(w http.ResponseWriter, r *http.Request) {}
`,
				"admin/admin.go": `package web

type Thing struct{}

//fabrik:provider
func NewThing() *Thing { return nil }
`,
			},
			`duplicate package name "web"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, diags := generate(t, tt.files)
			if !hasDiag(diags, tt.msg) {
				t.Fatalf("want diagnostic containing %q, got: %v", tt.msg, diags)
			}
			if !diags.HasFatal() {
				t.Fatalf("expected fatal diagnostics for %s", tt.name)
			}
		})
	}
}

func TestGenerateWarnsOnMainPackageDirective(t *testing.T) {
	src, diags := generate(t, map[string]string{
		"main.go": `package main

import "net/http"

//fabrik:http GET /
func Index(w http.ResponseWriter, r *http.Request) {}

func main() { _ = run() }
`,
	})
	if diags.HasFatal() {
		t.Fatalf("main-package directive should warn, not error: %v", diags)
	}
	if !hasDiag(diags, "in package main is ignored") {
		t.Fatalf("expected main-package warning, got: %v", diags)
	}
	if strings.Contains(src, "Index") {
		t.Errorf("main-package handler should not be wired:\n%s", src)
	}
}
