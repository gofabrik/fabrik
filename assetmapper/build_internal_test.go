package assetmapper

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// Build snapshots rewritten and pass-through content.
func TestBuildSnapshotsAllContent(t *testing.T) {
	fsys := fstest.MapFS{
		"style.css":     {Data: []byte(`body { background: url("./images/bg.png"); }`)},
		"images/bg.png": {Data: []byte("PNG")},
		"plain.js":      {Data: []byte("export function f() {}")},
	}
	c, err := Build([]Root{{FS: fsys}}, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	byLogical := make(map[string]serveEntry, len(c.entries))
	for _, e := range c.entries {
		byLogical[e.logical] = e
	}
	if byLogical["style.css"].content == nil {
		t.Fatal("rewritten CSS must be retained in memory")
	}
	if !strings.Contains(string(byLogical["style.css"].content), "/assets/images/bg-") {
		t.Fatalf("retained CSS %q lacks hashed image URL", byLogical["style.css"].content)
	}
	if string(byLogical["images/bg.png"].content) != "PNG" {
		t.Fatal("pass-through image was not snapshotted")
	}
	if string(byLogical["plain.js"].content) != "export function f() {}" {
		t.Fatal("unchanged JS was not snapshotted")
	}
}

func TestBuildSnapshotSurvivesSourceMutation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logo.png")
	if err := os.WriteFile(path, []byte("version one"), 0o600); err != nil {
		t.Fatal(err)
	}
	compiled, err := Build([]Root{{FS: os.DirFS(dir)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	url, err := compiled.Asset("logo.png")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("changed bytes with another length"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	compiled.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, url, nil))
	if response.Code != http.StatusOK || response.Body.String() != "version one" {
		t.Fatalf("snapshot response = %d %q", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Length"); got != "11" {
		t.Fatalf("Content-Length = %q, want 11", got)
	}
}

// Build rejects a source path that equals another asset's compiled path.
func TestBuildLiteralSourceCollision(t *testing.T) {
	content := []byte("export function f() {}")
	hashed := hashedName("app.js", hashContent(content))
	fsys := fstest.MapFS{
		"app.js": {Data: content},
		hashed:   {Data: []byte("impostor")},
	}
	_, err := Build([]Root{{FS: fsys}}, nil)
	if err == nil || !strings.Contains(err.Error(), "literal source path") {
		t.Fatalf("Build err = %v, want literal source path collision", err)
	}
}
