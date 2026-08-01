package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAssetsRequire verifies discovery plus the vendor round trip:
// the command finds the declared asset tree, downloads through the
// resolver, and leaves ordinary committed sources behind.
func TestAssetsRequire(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/generate":
			if _, err := fmt.Fprintf(w, `{"map":{"imports":{"htmx.org":"%s/npm:htmx.org@2.0.3/htmx.js"}}}`, srv.URL); err != nil {
				t.Errorf("write generate response: %v", err)
			}
		case strings.HasPrefix(r.URL.Path, "/npm:"):
			if _, err := fmt.Fprint(w, "export default {};\n"); err != nil {
				t.Errorf("write module response: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
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
	write("go.mod", "module app\n\ngo 1.27\n")
	write("main.go", "package main\n\nfunc main() {}\n")
	write("web/assets.go", "package web\n\nimport \"embed\"\n\n//fabrik:assets\n//go:embed all:assets\nvar Assets embed.FS\n")
	write("web/assets/app.js", "export {}\n")
	t.Chdir(dir)

	if err := assetsCmd([]string{
		"require",
		"-jspm", srv.URL,
		"-allow-http",
		"-allow-private-network",
		"htmx.org@2.0.3",
	}); err != nil {
		t.Fatalf("fabrik assets require: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "web/assets/vendor/htmx.org-*.js"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("vendored files = %v, %v; want one immutable artifact", matches, err)
	}
	artifact := matches[0]
	im, err := os.ReadFile(filepath.Join(dir, "web/assets/importmap.json")) // #nosec G304 -- reads a test-controlled temporary path
	if err != nil || !strings.Contains(string(im), `"htmx.org"`) {
		t.Fatalf("importmap.json = %q, %v", im, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "web/assets/vendor.lock.json")); err != nil {
		t.Fatalf("vendor lock missing: %v", err)
	}

	if err := assetsCmd([]string{"remove", "htmx.org"}); err != nil {
		t.Fatalf("fabrik assets remove: %v", err)
	}
	if _, err := os.Stat(artifact); err != nil {
		t.Fatalf("remove deleted immutable artifact before prune: %v", err)
	}
	if err := assetsCmd([]string{"prune"}); err != nil {
		t.Fatalf("fabrik assets prune: %v", err)
	}
	if _, err := os.Stat(artifact); !os.IsNotExist(err) {
		t.Fatalf("orphaned artifact survived prune: %v", err)
	}
}

func TestAssetsRequireNoDeclaration(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module app\n\ngo 1.27\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	err := assetsCmd([]string{"require", "htmx.org"})
	if err == nil || !strings.Contains(err.Error(), "no //fabrik:assets declaration") {
		t.Fatalf("err = %v, want missing-declaration error", err)
	}
}
