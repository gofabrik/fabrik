package assetmapper_test

import (
	"html/template"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/assetmapper"
)

var _ assetmapper.Server = (*assetmapper.Compiled)(nil)

func writeAsset(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sourceServer(t *testing.T, dir string) assetmapper.Server {
	t.Helper()
	rt, err := assetmapper.NewSource([]assetmapper.Root{{FS: os.DirFS(dir)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func TestNewSource_ServesEditsWithoutRestart(t *testing.T) {
	dir := t.TempDir()
	writeAsset(t, dir, "app.js", `console.log("one");`)
	rt := sourceServer(t, dir)

	before, err := rt.Asset("app.js")
	if err != nil {
		t.Fatal(err)
	}
	writeAsset(t, dir, "app.js", `console.log("two");`)
	after, err := rt.Asset("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatalf("URL did not change after edit: %s", before)
	}

	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, httptest.NewRequest("GET", after, nil))
	if !strings.Contains(rec.Body.String(), "two") {
		t.Fatalf("served content did not follow the edit: %q", rec.Body.String())
	}
}

func TestNewSource_RejectsMissingRoot(t *testing.T) {
	_, err := assetmapper.NewSource([]assetmapper.Root{
		{FS: os.DirFS(filepath.Join(t.TempDir(), "absent"))},
	}, nil)
	if err == nil {
		t.Fatal("expected error for a missing root directory")
	}
	if !strings.Contains(err.Error(), "working directory") {
		t.Fatalf("error does not name the working-directory contract: %v", err)
	}
}

func TestNewSource_RejectsNonDirectoryRoot(t *testing.T) {
	dir := t.TempDir()
	writeAsset(t, dir, "file.txt", "not a directory")
	_, err := assetmapper.NewSource([]assetmapper.Root{
		{FS: os.DirFS(filepath.Join(dir, "file.txt"))},
	}, nil)
	if err == nil {
		t.Fatal("expected error for a non-directory root")
	}
}

func TestNewSource_RejectsUnreadableRoot(t *testing.T) {
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	if _, err := os.ReadDir(locked); err == nil {
		t.Skip("directory is readable despite 0o000 (running as root)")
	}
	_, err := assetmapper.NewSource([]assetmapper.Root{{FS: os.DirFS(locked)}}, nil)
	if err == nil {
		t.Fatal("expected error for an unreadable root")
	}
}

func TestNewSource_SnapshotsImportmap(t *testing.T) {
	dir := t.TempDir()
	writeAsset(t, dir, "app.js", "export {}")
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js"}
	rt, err := assetmapper.NewSource([]assetmapper.Root{{FS: os.DirFS(dir)}}, im)
	if err != nil {
		t.Fatal(err)
	}
	im.Entries["late"] = assetmapper.ImportmapEntry{Path: "app.js"}

	render := rt.FuncMap()["importmap"].(func(...string) (template.HTML, error))
	html, err := render()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(html), `"app"`) {
		t.Fatalf("construction-time entry is missing from rendering: %s", html)
	}
	if strings.Contains(string(html), "late") {
		t.Fatalf("post-construction entry mutation reached rendering: %s", html)
	}
}

func TestNewSource_ETagRevalidatesAcrossEdit(t *testing.T) {
	dir := t.TempDir()
	writeAsset(t, dir, "app.js", "one")
	rt := sourceServer(t, dir)
	url, err := rt.Asset("app.js")
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, httptest.NewRequest("GET", url, nil))
	oldETag := rec.Header().Get("ETag")

	writeAsset(t, dir, "app.js", "two")
	req := httptest.NewRequest("GET", url, nil)
	req.Header.Set("If-None-Match", oldETag)
	rec = httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Body.String() != "two" {
		t.Fatalf("stale ETag: got %d %q, want 200 with the new body", rec.Code, rec.Body.String())
	}
	newETag := rec.Header().Get("ETag")
	if newETag == oldETag {
		t.Fatal("ETag did not change with the content")
	}

	req = httptest.NewRequest("GET", url, nil)
	req.Header.Set("If-None-Match", newETag)
	rec = httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, req)
	if rec.Code != 304 {
		t.Fatalf("fresh ETag: got %d, want 304", rec.Code)
	}
}

func TestNewSource_URLPrefixParityWithCompiled(t *testing.T) {
	dir := t.TempDir()
	writeAsset(t, dir, "app.js", "export {}")
	roots := []assetmapper.Root{{FS: os.DirFS(dir)}}

	src, err := assetmapper.NewSource(roots, nil, assetmapper.WithURLPrefix("/static/"))
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := assetmapper.Build(roots, nil, assetmapper.WithURLPrefix("/static/"))
	if err != nil {
		t.Fatal(err)
	}
	for name, rt := range map[string]assetmapper.Server{"source": src, "compiled": compiled} {
		u, err := rt.Asset("app.js")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(u, "/static/") {
			t.Errorf("%s URL = %q, want the /static/ prefix", name, u)
		}
		rec := httptest.NewRecorder()
		rt.Handler().ServeHTTP(rec, httptest.NewRequest("GET", u, nil))
		if rec.Code != 200 {
			t.Errorf("%s GET %s = %d, want 200", name, u, rec.Code)
		}
	}
}

func TestNewSource_ImportmapCSPSourcesRelaxed(t *testing.T) {
	dir := t.TempDir()
	writeAsset(t, dir, "app.js", "export {}")
	srcs := sourceServer(t, dir).ImportmapCSPSources()
	if len(srcs) != 1 || srcs[0] != "'unsafe-inline'" {
		t.Fatalf("source mode CSP sources = %v, want the explicit inline relaxation", srcs)
	}
}

func TestNewSource_FuncMapCarriesAssetHelpers(t *testing.T) {
	dir := t.TempDir()
	writeAsset(t, dir, "app.js", "export {}")
	fm := sourceServer(t, dir).FuncMap()
	for _, name := range []string{"asset", "importmap", "module_preload_links"} {
		if _, ok := fm[name]; !ok {
			t.Errorf("FuncMap is missing %q", name)
		}
	}
}

func TestNewSource_DiscoversImportmapFromRoots(t *testing.T) {
	dir := t.TempDir()
	writeAsset(t, dir, "app.js", "export {}")
	writeAsset(t, dir, "importmap.json", `{"app":{"path":"app.js"}}`)
	rt := sourceServer(t, dir)

	render := rt.FuncMap()["importmap"].(func(...string) (template.HTML, error))
	html, err := render()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(html), `"app"`) {
		t.Fatalf("rendered importmap is missing the discovered entry: %s", html)
	}
}

func TestCompiled_ImportmapCSPSources(t *testing.T) {
	dir := t.TempDir()
	writeAsset(t, dir, "app.js", "export {}")
	c, err := assetmapper.Build([]assetmapper.Root{{FS: os.DirFS(dir)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	srcs := c.ImportmapCSPSources()
	if len(srcs) != 1 || srcs[0] != c.CSPImportmapHash() {
		t.Fatalf("ImportmapCSPSources = %v, want the importmap hash %q", srcs, c.CSPImportmapHash())
	}
}

func TestRuntimeConfig_Mode(t *testing.T) {
	cases := []struct {
		kind string
		want assetmapper.Kind
		err  bool
	}{
		{"", assetmapper.KindCompiled, false},
		{"compiled", assetmapper.KindCompiled, false},
		{"source", assetmapper.KindSource, false},
		{"bundler", "", true},
	}
	for _, c := range cases {
		got, err := assetmapper.Options{Kind: c.kind}.Mode()
		if c.err {
			if err == nil {
				t.Errorf("Kind %q: expected error", c.kind)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("Kind %q: got %q, %v; want %q", c.kind, got, err, c.want)
		}
	}
}
