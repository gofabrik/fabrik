package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// jspmMirror serves a package and one transitive dependency.
func jspmMirror(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/generate":
			if _, err := fmt.Fprintf(w, `{"map":{"imports":{"htmx.org":"%s/npm:htmx.org@2.0.3/htmx.js"},"scopes":{"./":{"idiomorph":"%s/npm:idiomorph@0.3.0/idiomorph.js"}}}}`,
				srv.URL, srv.URL); err != nil {
				t.Errorf("write generate response: %v", err)
			}
		case strings.HasPrefix(r.URL.Path, "/npm:"):
			if _, err := fmt.Fprint(w, "export default {};\n"); err != nil {
				t.Errorf("write package response: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRequireRemovePrune(t *testing.T) {
	srv := jspmMirror(t)
	dir := filepath.Join(t.TempDir(), "assets")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := run([]string{"require", "-dir", dir, "-jspm", srv.URL, "htmx.org@2.0.3"}, &out); err != nil {
		t.Fatalf("require: %v", err)
	}
	for _, want := range []string{"vendored htmx.org 2.0.3", "vendored idiomorph 0.3.0"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("require output %q lacks %q", out.String(), want)
		}
	}
	for _, f := range []string{"vendor/htmx.org.js", "vendor/idiomorph.js", "importmap.json"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing %s after require: %v", f, err)
		}
	}
	im, err := os.ReadFile(filepath.Join(dir, "importmap.json")) // #nosec G304 -- reads an app-selected asset path
	if err != nil || !strings.Contains(string(im), `"htmx.org"`) {
		t.Fatalf("importmap.json = %q, %v", im, err)
	}

	out.Reset()
	if err := run([]string{"remove", "-dir", dir, "htmx.org"}, &out); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "vendor/htmx.org.js")); !os.IsNotExist(err) {
		t.Error("vendor/htmx.org.js survived remove")
	}

	// Prune removes files orphaned by hand-edited importmaps.
	if err := os.WriteFile(filepath.Join(dir, "importmap.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := run([]string{"prune", "-dir", dir}, &out); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if !strings.Contains(out.String(), "pruned idiomorph.js") {
		t.Errorf("prune output %q lacks orphaned dependency", out.String())
	}
}

// Completed packages stay consistent when a later package fails.
func TestRequirePartialFailure(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/generate":
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), "bad") {
				http.Error(w, "no such package", http.StatusInternalServerError)
				return
			}
			if _, err := fmt.Fprintf(w, `{"map":{"imports":{"good":"%s/npm:good@1.0.0/good.js"}}}`, srv.URL); err != nil {
				t.Errorf("write generate response: %v", err)
			}
		case strings.HasPrefix(r.URL.Path, "/npm:"):
			if _, err := fmt.Fprint(w, "export default {};\n"); err != nil {
				t.Errorf("write package response: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := filepath.Join(t.TempDir(), "assets")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"require", "-dir", dir, "-jspm", srv.URL, "good@1.0.0", "bad@1.0.0"}, &out); err == nil {
		t.Fatal("require good bad: want error from bad")
	}
	if _, err := os.Stat(filepath.Join(dir, "vendor/good.js")); err != nil {
		t.Fatalf("good's file missing after partial failure: %v", err)
	}
	im, err := os.ReadFile(filepath.Join(dir, "importmap.json")) // #nosec G304 -- reads an app-selected asset path
	if err != nil || !strings.Contains(string(im), `"good"`) {
		t.Fatalf("good's entry not committed before the failure: %q, %v", im, err)
	}
}

// A bad remove target aborts before any file or entry changes.
func TestRemoveValidatesBeforeDeleting(t *testing.T) {
	srv := jspmMirror(t)
	dir := filepath.Join(t.TempDir(), "assets")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"require", "-dir", dir, "-jspm", srv.URL, "htmx.org"}, &out); err != nil {
		t.Fatal(err)
	}

	if err := run([]string{"remove", "-dir", dir, "htmx.org", "typo"}, &out); err == nil || !strings.Contains(err.Error(), `"typo" not in importmap`) {
		t.Fatalf("remove with typo: err = %v, want not-in-importmap", err)
	}
	assertUntouched := func(t *testing.T, why string) {
		t.Helper()
		if _, err := os.Stat(filepath.Join(dir, "vendor/htmx.org.js")); err != nil {
			t.Fatalf("valid package was deleted despite %s aborting the batch", why)
		}
		im, _ := os.ReadFile(filepath.Join(dir, "importmap.json")) // #nosec G304 -- reads an app-selected asset path
		if !strings.Contains(string(im), `"htmx.org"`) {
			t.Fatalf("valid entry was dropped despite %s aborting the batch", why)
		}
	}
	assertUntouched(t, "the typo")

	// Unsafe hand-edited keys fail during batch preflight.
	im, err := os.ReadFile(filepath.Join(dir, "importmap.json")) // #nosec G304 -- reads an app-selected asset path
	if err != nil {
		t.Fatal(err)
	}
	evil := strings.Replace(string(im), "{\n", "{\n  \"../evil\": {\"version\": \"1.0.0\"},\n", 1)
	if err := os.WriteFile(filepath.Join(dir, "importmap.json"), []byte(evil), 0o600); err != nil { // #nosec G703 -- test writes a crafted importmap to an app-selected path
		t.Fatal(err)
	}
	if err := run([]string{"remove", "-dir", dir, "htmx.org", "../evil"}, &out); err == nil || !strings.Contains(err.Error(), "safe path") {
		t.Fatalf("remove with unsafe key: err = %v, want safe-path rejection", err)
	}
	assertUntouched(t, "the unsafe key")
}

func TestRunErrors(t *testing.T) {
	var out bytes.Buffer
	if err := run(nil, &out); err == nil {
		t.Error("no args: want usage error")
	}
	if err := run([]string{"require", "-dir", filepath.Join(t.TempDir(), "missing"), "x"}, &out); err == nil {
		t.Error("missing dir: want error")
	}
	if err := run([]string{"frobnicate"}, &out); err == nil {
		t.Error("unknown subcommand: want error")
	}
}

func TestSplitPackageVersion(t *testing.T) {
	for in, want := range map[string][2]string{
		"htmx.org":         {"htmx.org", ""},
		"htmx.org@2.0.3":   {"htmx.org", "2.0.3"},
		"@scope/pkg":       {"@scope/pkg", ""},
		"@scope/pkg@1.0.0": {"@scope/pkg", "1.0.0"},
	} {
		pkg, version := splitPackageVersion(in)
		if pkg != want[0] || version != want[1] {
			t.Errorf("splitPackageVersion(%q) = %q, %q; want %q, %q", in, pkg, version, want[0], want[1])
		}
	}
}
