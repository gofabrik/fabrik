package assetmapper

import (
	"strings"
	"testing"
	"testing/fstest"
)

// Build retains only content changed by rewriting.
func TestBuildMemoryDiscipline(t *testing.T) {
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
	if byLogical["style.css"].rewritten == nil {
		t.Fatal("rewritten CSS must be retained in memory")
	}
	if !strings.Contains(string(byLogical["style.css"].rewritten), "/assets/images/bg-") {
		t.Fatalf("retained CSS %q lacks hashed image URL", byLogical["style.css"].rewritten)
	}
	if byLogical["images/bg.png"].rewritten != nil {
		t.Fatal("pass-through image must not be duplicated into memory")
	}
	if byLogical["plain.js"].rewritten != nil {
		t.Fatal("JS with no rewritten references must not be duplicated into memory")
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
