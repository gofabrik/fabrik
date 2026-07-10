package assetmapper

import (
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
