package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWireAssetOverlay pins that asset validation reads unsaved
// editor buffers: importmap.json edits and new asset files validate
// through the overlay, since neither ever requires a rewire.
func TestWireAssetOverlay(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	assetsDir, err := filepath.Abs("../../../assetmapper")
	if err != nil {
		t.Fatal(err)
	}
	write := func(rel, content string) {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module app\n\ngo 1.26\n\nrequire github.com/gofabrik/fabrik/assetmapper v0.0.0\n\nreplace github.com/gofabrik/fabrik/assetmapper => "+assetsDir+"\n")
	write("main.go", "package main\n\nfunc main() { _ = run }\n")
	write("shared/assets.go", "package shared\n\nimport \"embed\"\n\n//fabrik:assets\n//go:embed all:assets\nvar Assets embed.FS\n")
	write("shared/assets/style.css", "body {}\n")
	write("web/assets.go", "package web\n\nimport \"embed\"\n\n//fabrik:assets\n//go:embed all:assets\nvar Assets embed.FS\n")
	write("web/assets/app.js", "export {}\n")
	write("web/assets/importmap.json", `{"app": {"path": "app.js", "entrypoint": true}}`)

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
		t.Fatalf("overlay asset error %q not surfaced: %v", needle, res.Diags)
	}

	// An unsaved importmap.json edit pointing at a missing asset.
	wantDiag(t, map[string][]byte{
		filepath.Join(dir, "web/assets/importmap.json"): []byte(`{"app": {"path": "missing.js", "entrypoint": true}}`),
	}, "not a known asset")

	// An unsaved importmap.json edit that is not valid JSON.
	wantDiag(t, map[string][]byte{
		filepath.Join(dir, "web/assets/importmap.json"): []byte(`{"app": {`),
	}, "ParseImportmap")

	// A new unsaved asset file colliding with another package's tree.
	wantDiag(t, map[string][]byte{
		filepath.Join(dir, "web/assets/style.css"): []byte("p {}\n"),
	}, "already provided")
}
