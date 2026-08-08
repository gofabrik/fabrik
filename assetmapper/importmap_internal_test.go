package assetmapper

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
	"testing/fstest"
)

// preloadCacheSize is test-only inspection for the prod preload cache.
func preloadCacheSize(im *Importmap) int {
	n := 0
	im.preloadCache.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

func TestImportmap_PreloadGraphCached_ProdMode(t *testing.T) {
	// Prod mode memoises per mapper and entrypoint tuple.
	src := fstest.MapFS{
		"app.js":  {Data: []byte(`import u from "./util.js";`)},
		"util.js": {Data: []byte(`export default {}`)},
	}
	manifest := &Manifest{
		URLPrefix: "/assets/",
		Entries: map[string]string{
			"app.js":  "app-deadbeef.js",
			"util.js": "util-cafef00d.js",
		},
	}
	m, err := New(Config{
		Roots:    []Root{{FS: src}},
		Manifest: manifest,
	})
	if err != nil {
		t.Fatal(err)
	}

	im := NewImportmap()
	im.Entries["app"] = ImportmapEntry{Path: "app.js", Entrypoint: true}

	if got := preloadCacheSize(im); got != 0 {
		t.Errorf("cache size before first call = %d, want 0", got)
	}

	first, err := im.preloadGraph(m, []string{"app"})
	if err != nil {
		t.Fatal(err)
	}
	if got := preloadCacheSize(im); got != 1 {
		t.Errorf("cache size after first call = %d, want 1", got)
	}

	second, err := im.preloadGraph(m, []string{"app"})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.JSURLs) != len(second.JSURLs) {
		t.Errorf("cached JSURLs length differs: %d vs %d", len(first.JSURLs), len(second.JSURLs))
	}
	for i := range first.JSURLs {
		if first.JSURLs[i] != second.JSURLs[i] {
			t.Errorf("cached JSURLs differ at [%d]: %q vs %q", i, first.JSURLs[i], second.JSURLs[i])
		}
	}
	// Same key keeps one entry.
	if got := preloadCacheSize(im); got != 1 {
		t.Errorf("cache size after second call = %d, want still 1", got)
	}

	// Different entrypoint key creates a second cache entry.
	im.Entries["other"] = ImportmapEntry{Path: "util.js", Entrypoint: true}
	if _, err := im.preloadGraph(m, []string{"other"}); err != nil {
		t.Fatal(err)
	}
	if got := preloadCacheSize(im); got != 2 {
		t.Errorf("cache size after distinct-key call = %d, want 2", got)
	}
}

func TestImportmap_PreloadGraphNotCached_DevMode(t *testing.T) {
	// Dev mode does not cache preload graphs.
	src := fstest.MapFS{"app.js": {Data: []byte(`export default {}`)}}
	m, err := New(Config{Roots: []Root{{FS: src}}})
	if err != nil {
		t.Fatal(err)
	}

	im := NewImportmap()
	im.Entries["app"] = ImportmapEntry{Path: "app.js", Entrypoint: true}

	if _, err := im.preloadGraph(m, []string{"app"}); err != nil {
		t.Fatal(err)
	}
	if got := preloadCacheSize(im); got != 0 {
		t.Errorf("dev mode populated the cache (size = %d); want 0", got)
	}
}

func TestImportmap_PreloadGraphCacheSeparatePerMapper(t *testing.T) {
	// Distinct Mappers get distinct cache entries.
	src := fstest.MapFS{"app.js": {Data: []byte(`export default {}`)}}
	manifest := &Manifest{
		URLPrefix: "/assets/",
		Entries:   map[string]string{"app.js": "app-deadbeef.js"},
	}
	m1, _ := New(Config{Roots: []Root{{FS: src}}, Manifest: manifest})
	m2, _ := New(Config{Roots: []Root{{FS: src}}, Manifest: manifest})

	im := NewImportmap()
	im.Entries["app"] = ImportmapEntry{Path: "app.js", Entrypoint: true}

	if _, err := im.preloadGraph(m1, []string{"app"}); err != nil {
		t.Fatal(err)
	}
	if _, err := im.preloadGraph(m2, []string{"app"}); err != nil {
		t.Fatal(err)
	}
	if got := preloadCacheSize(im); got != 2 {
		t.Errorf("cache size = %d, want 2 (one per Mapper instance)", got)
	}
}

func TestImportmapRenderer_CacheIsScopedToSnapshot(t *testing.T) {
	src := fstest.MapFS{
		"app.js":   {Data: []byte("export {}")},
		"other.js": {Data: []byte("export {}")},
	}
	manifest := NewManifest()
	manifest.Entries["app.js"] = "app-deadbeef.js"
	manifest.Entries["other.js"] = "other-cafef00d.js"
	manifest.Dependencies = map[string][]string{
		"app.js":   nil,
		"other.js": nil,
	}
	mapper, err := New(Config{
		Roots:    []Root{{FS: src}},
		Manifest: manifest,
	})
	if err != nil {
		t.Fatal(err)
	}
	im := NewImportmap()
	im.Entries["entry"] = ImportmapEntry{Path: "app.js", Entrypoint: true}
	renderer := im.Bind(mapper)

	first, err := renderer.ModulePreloadLinks("entry")
	if err != nil {
		t.Fatal(err)
	}
	if got := preloadCacheSize(renderer.importmap); got != 1 {
		t.Fatalf("bound cache size = %d, want 1", got)
	}

	im.Entries["entry"] = ImportmapEntry{Path: "other.js", Entrypoint: true}
	second, err := renderer.ModulePreloadLinks("entry")
	if err != nil {
		t.Fatal(err)
	}
	if first != second || !strings.Contains(second, "app-deadbeef.js") {
		t.Fatalf("bound cache changed after builder mutation:\nfirst: %s\nsecond: %s", first, second)
	}
}

func TestImportmap_PartialManifestFallsBackToSourceGraph(t *testing.T) {
	src := fstest.MapFS{
		"app.js": {Data: []byte(`import "./dep.js";`)},
		"dep.js": {Data: []byte("export {}")},
	}
	manifest := NewManifest()
	manifest.Entries["app.js"] = "app-deadbeef.js"
	manifest.Entries["dep.js"] = "dep-cafef00d.js"
	mapper, err := New(Config{
		Roots:    []Root{{FS: src}},
		Manifest: manifest,
	})
	if err != nil {
		t.Fatal(err)
	}
	im := NewImportmap()
	im.Entries["app"] = ImportmapEntry{Path: "app.js", Entrypoint: true}

	graph, err := im.preloadGraph(mapper, []string{"app"})
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.JSURLs) != 2 {
		t.Fatalf("partial manifest preload URLs = %v, want app and dep", graph.JSURLs)
	}
}

// The rendered body and CSP hash must cover the same escaped bytes.
func TestRenderEscapesHostileSpecifiersAndKeepsCSPCoupling(t *testing.T) {
	src := fstest.MapFS{
		"x.js": {Data: []byte("x")},
		"y.js": {Data: []byte("y")},
		"z.js": {Data: []byte("z")},
	}
	m, err := New(Config{Roots: []Root{{FS: src}}})
	if err != nil {
		t.Fatal(err)
	}
	im := &Importmap{Entries: map[string]ImportmapEntry{
		"</script><script>alert(1)</script>": {Path: "x.js"},
		"line\u2028sep\u2029end":             {Path: "y.js"},
		"bad\xffutf8":                        {Path: "z.js"},
	}}
	page, err := im.Render(m)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	start := strings.Index(page, ">") + 1
	end := strings.Index(page, "</script>")
	if start <= 0 || end < start {
		t.Fatalf("no inline script body in:\n%s", page)
	}
	body := page[start:end]
	for _, raw := range []string{"</script>", "\u2028", "\u2029"} {
		if strings.Contains(body, raw) {
			t.Fatalf("rendered body contains raw %q:\n%s", raw, body)
		}
	}
	sum := sha256.Sum256([]byte(body))
	want := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	got, err := im.importmapBodyHash(m)
	if err != nil {
		t.Fatalf("importmapBodyHash: %v", err)
	}
	if got != want {
		t.Fatalf("CSP hash %q does not cover the rendered body bytes (want %q)", got, want)
	}
}
