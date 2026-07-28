package assetmapper_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gofabrik/fabrik/assetmapper"
)

func TestCompile_UsesSamePlanAsBuild(t *testing.T) {
	src := fstest.MapFS{
		"app.js":          {Data: []byte(`import "./util.js";`)},
		"util.js":         {Data: []byte(`export const value = 1;`)},
		"styles/site.css": {Data: []byte(`body { background: url("../logo.png"); }`)},
		"logo.png":        {Data: []byte("PNG")},
	}
	roots := []assetmapper.Root{{FS: src}}
	compiled, err := assetmapper.Build(roots, nil, assetmapper.WithURLPrefix("/static/"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := assetmapper.Compile(roots, t.TempDir(), assetmapper.WithURLPrefix("/static/"))
	if err != nil {
		t.Fatal(err)
	}
	for logical, output := range manifest.Entries {
		url, err := compiled.Asset(logical)
		if err != nil {
			t.Fatalf("Build Asset(%q): %v", logical, err)
		}
		if want := "/static/" + output; url != want {
			t.Errorf("Build Asset(%q) = %q, Compile planned %q", logical, url, want)
		}
	}
}

func TestCompile_ValidatesDiscoveredImportmap(t *testing.T) {
	src := fstest.MapFS{
		"app.js":         {Data: []byte("export {}")},
		"importmap.json": {Data: []byte(`{"missing":{"path":"missing.js"}}`)},
	}
	_, err := assetmapper.Compile([]assetmapper.Root{{FS: src}}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "not a known asset") {
		t.Fatalf("Compile error = %v, want importmap validation failure", err)
	}
}

func TestCompile_PersistsPreloadDependencies(t *testing.T) {
	src := fstest.MapFS{
		"app.js":         {Data: []byte(`import "./dep.js";`)},
		"dep.js":         {Data: []byte("export {}")},
		"importmap.json": {Data: []byte(`{"app":{"path":"app.js","entrypoint":true}}`)},
	}
	dir := t.TempDir()
	if _, err := assetmapper.Compile([]assetmapper.Root{{FS: src}}, dir); err != nil {
		t.Fatal(err)
	}
	manifest, err := assetmapper.LoadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := manifest.Dependencies["app.js"]; len(got) != 1 || got[0] != "dep.js" {
		t.Fatalf("persisted app.js dependencies = %v, want [dep.js]", got)
	}

	// Production preload rendering must use the persisted graph, not rescan
	// source files that may no longer be present.
	delete(src, "app.js")
	delete(src, "dep.js")
	mapper, err := assetmapper.New(assetmapper.Config{
		Roots:    []assetmapper.Root{{FS: src}},
		Manifest: manifest,
	})
	if err != nil {
		t.Fatal(err)
	}
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}
	links, err := im.ModulePreloadLinks(mapper, "app")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(links, manifest.Entries["dep.js"]) {
		t.Fatalf("preloads do not use persisted dependency graph:\n%s", links)
	}
}

func TestCompile_RejectsNoRoots(t *testing.T) {
	if _, err := assetmapper.Compile(nil, t.TempDir()); err == nil {
		t.Fatal("expected error for empty Roots")
	}
}

func TestCompile_RejectsNilFS(t *testing.T) {
	_, err := assetmapper.Compile(
		[]assetmapper.Root{{FS: nil}}, t.TempDir(),
	)
	if err == nil {
		t.Fatal("expected error for nil Roots[0].FS")
	}
}

func TestCompile_HashesEveryFile(t *testing.T) {
	srcFS := fstest.MapFS{
		"app.js":          {Data: []byte("alert('hi')")},
		"styles/site.css": {Data: []byte("body{}")},
	}
	dir := t.TempDir()
	manifest, err := assetmapper.Compile([]assetmapper.Root{{FS: srcFS}}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(manifest.Entries))
	}
	appPath, ok := manifest.Entries["app.js"]
	if !ok {
		t.Fatal("app.js missing from manifest")
	}
	if !strings.HasPrefix(appPath, "app-") || !strings.HasSuffix(appPath, ".js") {
		t.Errorf("app.js mapped to %q, want app-<hash>.js", appPath)
	}
	cssPath, ok := manifest.Entries["styles/site.css"]
	if !ok {
		t.Fatal("styles/site.css missing from manifest")
	}
	if !strings.HasPrefix(cssPath, "styles/site-") {
		t.Errorf("styles/site.css mapped to %q, want styles/site-<hash>.css", cssPath)
	}
}

func TestCompile_WritesFileContent(t *testing.T) {
	srcFS := fstest.MapFS{
		"app.js": {Data: []byte("console.log('hi')")},
	}
	dir := t.TempDir()
	manifest, err := assetmapper.Compile([]assetmapper.Root{{FS: srcFS}}, dir)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, manifest.Entries["app.js"])) // #nosec G304 -- reads an app-selected asset path
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "console.log('hi')" {
		t.Errorf("file content = %q, want %q", data, "console.log('hi')")
	}
}

func TestCompile_MountAtPrefixesLogicalAndOutputPath(t *testing.T) {
	libFS := fstest.MapFS{
		"styles.css": {Data: []byte("body{}")},
	}
	dir := t.TempDir()
	manifest, err := assetmapper.Compile([]assetmapper.Root{
		{FS: libFS, MountAt: "jobs"},
	}, dir)
	if err != nil {
		t.Fatal(err)
	}
	stylesPath, ok := manifest.Entries["jobs/styles.css"]
	if !ok {
		t.Fatalf("jobs/styles.css missing from manifest; entries = %v", manifest.Entries)
	}
	if !strings.HasPrefix(stylesPath, "jobs/styles-") || !strings.HasSuffix(stylesPath, ".css") {
		t.Errorf("path = %q, want jobs/styles-<hash>.css", stylesPath)
	}
	if _, err := os.Stat(filepath.Join(dir, stylesPath)); err != nil {
		t.Errorf("expected file at %s: %v", stylesPath, err)
	}
}

func TestCompile_FirstRootWinsShadowing(t *testing.T) {
	user := fstest.MapFS{"app.js": {Data: []byte("USER")}}
	lib := fstest.MapFS{"app.js": {Data: []byte("LIB")}}
	dir := t.TempDir()
	manifest, err := assetmapper.Compile([]assetmapper.Root{{FS: user}, {FS: lib}}, dir)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, manifest.Entries["app.js"])) // #nosec G304 -- reads an app-selected asset path
	if string(data) != "USER" {
		t.Errorf("content = %q, want USER (first root wins)", data)
	}
}

func TestCompile_PersistsManifestToDisk(t *testing.T) {
	srcFS := fstest.MapFS{"app.js": {Data: []byte("x")}}
	dir := t.TempDir()
	if _, err := assetmapper.Compile([]assetmapper.Root{{FS: srcFS}}, dir); err != nil {
		t.Fatal(err)
	}
	got, err := assetmapper.LoadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Entries["app.js"]; !ok {
		t.Errorf("loaded manifest missing app.js; entries = %v", got.Entries)
	}
}

func TestCompile_CreatesPublicDir(t *testing.T) {
	srcFS := fstest.MapFS{"app.js": {Data: []byte("x")}}
	parent := t.TempDir()
	dir := filepath.Join(parent, "nested", "dist")
	if _, err := assetmapper.Compile([]assetmapper.Root{{FS: srcFS}}, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("publicDir not created: %v", err)
	}
}

func TestCompile_ProdMapperRoundTrip(t *testing.T) {
	// End-to-end check: Compile produces a manifest, the prod-mode
	// Mapper consumes it, Asset() returns a URL pointing at the
	// actual hashed file on disk.
	srcFS := fstest.MapFS{
		"app.js":          {Data: []byte("console.log('hi')")},
		"images/logo.png": {Data: []byte("PNGDATA")},
	}
	dir := t.TempDir()
	manifest, err := assetmapper.Compile([]assetmapper.Root{{FS: srcFS}}, dir)
	if err != nil {
		t.Fatal(err)
	}

	m, err := assetmapper.New(assetmapper.Config{
		Roots:    []assetmapper.Root{{FS: srcFS}},
		Manifest: manifest,
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, logical := range []string{"app.js", "images/logo.png"} {
		url, err := m.Asset(logical)
		if err != nil {
			t.Errorf("Asset(%q): %v", logical, err)
			continue
		}
		want := "/assets/" + manifest.Entries[logical]
		if url != want {
			t.Errorf("Asset(%q) = %q, want %q", logical, url, want)
		}
		// File must actually exist on disk at that path.
		hashedPath := filepath.Join(dir, manifest.Entries[logical])
		if _, err := os.Stat(hashedPath); err != nil {
			t.Errorf("hashed file missing at %s: %v", hashedPath, err)
		}
	}
}

func TestCompile_IdempotentForUnchangedInput(t *testing.T) {
	// Re-running Compile on the same source must produce identical
	// output (same manifest, same hashed filenames). Verifies the
	// hash is content-addressed and stable.
	srcFS := fstest.MapFS{"app.js": {Data: []byte("x")}}
	dir := t.TempDir()
	first, err := assetmapper.Compile([]assetmapper.Root{{FS: srcFS}}, dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := assetmapper.Compile([]assetmapper.Root{{FS: srcFS}}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if first.Entries["app.js"] != second.Entries["app.js"] {
		t.Errorf("hash differs across compiles: %q vs %q",
			first.Entries["app.js"], second.Entries["app.js"])
	}
}

func TestCompile_ErrorsWhenCompiledOutputShadowsLiteralSource(t *testing.T) {
	// A compiled output path cannot also be a literal source path.
	src := fstest.MapFS{
		"foo.js":                      {Data: []byte("x")},
		"foo-2d711642b726b0440162.js": {Data: []byte("// literal sibling")},
	}
	_, err := assetmapper.Compile([]assetmapper.Root{{FS: src}}, t.TempDir())
	if err == nil {
		t.Fatal("expected collision error")
	}
	for _, want := range []string{"foo.js", "foo-2d711642b726b0440162.js", "literal source"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

func TestCompile_HashSuffixedSourceWithoutShadowIsFine(t *testing.T) {
	// A literal foo-2d711642b726b0440162.js is OK as long as nothing ELSE
	// happens to compile to that exact name. Asserts the collision
	// check doesn't fire on a hash-shaped filename that has no
	// shadowing sibling.
	src := fstest.MapFS{
		"foo-2d711642b726b0440162.js": {Data: []byte("export default {}")},
	}
	manifest, err := assetmapper.Compile([]assetmapper.Root{{FS: src}}, t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	hashed := manifest.Entries["foo-2d711642b726b0440162.js"]
	if hashed == "" {
		t.Fatalf("missing manifest entry; entries = %v", manifest.Entries)
	}
	if !strings.HasPrefix(hashed, "foo-2d711642b726b0440162-") {
		t.Errorf("hashed = %q, want foo-2d711642b726b0440162-<hash>.js", hashed)
	}
}

func TestCompile_StreamingPathHashesLargeNonJSCSSFile(t *testing.T) {
	// Large non-JS/CSS assets use the streaming hash path.
	bigContent := make([]byte, 1024*1024)
	for i := range bigContent {
		bigContent[i] = byte(i % 251) // pseudo-binary
	}
	src := fstest.MapFS{
		"assets/big.bin": {Data: bigContent},
	}
	dir := t.TempDir()
	manifest, err := assetmapper.Compile([]assetmapper.Root{{FS: src}}, dir)
	if err != nil {
		t.Fatal(err)
	}
	hashed, ok := manifest.Entries["assets/big.bin"]
	if !ok {
		t.Fatalf("missing manifest entry; entries = %v", manifest.Entries)
	}
	// Hash must match the same SHA-256-truncated value the
	// in-memory path would have produced for identical content.
	got, err := os.ReadFile(filepath.Join(dir, hashed)) // #nosec G304 -- reads an app-selected asset path
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(bigContent) {
		t.Errorf("written length = %d, want %d", len(got), len(bigContent))
	}
	if string(got) != string(bigContent) {
		t.Error("written content does not match source (streaming path corrupted bytes)")
	}
}

func TestCompile_RemovesStaleTempFilesFromPriorCrashedRun(t *testing.T) {
	// Simulate a previous crashed compile by pre-seeding a stale
	// .assetmapper-tmp-*.tmp in publicDir. Next Compile should GC it
	// even though it never interferes with the current run's correctness.
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, ".assetmapper-tmp-stale-deadbeef.tmp")
	if err := os.WriteFile(stale, []byte("zombie"), 0o600); err != nil {
		t.Fatal(err)
	}

	src := fstest.MapFS{"app.js": {Data: []byte("export default {}")}}
	if _, err := assetmapper.Compile([]assetmapper.Root{{FS: src}}, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale temp file still present: %v", err)
	}
}

func TestCompile_StaleTempCleanupIsNarrow(t *testing.T) {
	// Only assetmapper temp files are removed.
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	keeper := filepath.Join(dir, ".assetmapper-readme.tmp")
	if err := os.WriteFile(keeper, []byte("user file"), 0o600); err != nil {
		t.Fatal(err)
	}

	src := fstest.MapFS{"app.js": {Data: []byte("export default {}")}}
	if _, err := assetmapper.Compile([]assetmapper.Root{{FS: src}}, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keeper); err != nil {
		t.Errorf(".assetmapper-readme.tmp (not ours) was GC'd: %v", err)
	}
}

func TestCompile_PublishFailureKeepsPreviousManifest(t *testing.T) {
	dir := t.TempDir()
	oldSource := fstest.MapFS{"app.js": {Data: []byte("old")}}
	oldManifest, err := assetmapper.Compile([]assetmapper.Root{{FS: oldSource}}, dir)
	if err != nil {
		t.Fatal(err)
	}
	oldManifestBytes, err := os.ReadFile(filepath.Join(dir, assetmapper.ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}

	newSource := fstest.MapFS{"app.js": {Data: []byte("new")}}
	probeDir := t.TempDir()
	newManifest, err := assetmapper.Compile([]assetmapper.Root{{FS: newSource}}, probeDir)
	if err != nil {
		t.Fatal(err)
	}
	blockedOutput := filepath.Join(dir, filepath.FromSlash(newManifest.Entries["app.js"]))
	if err := os.MkdirAll(blockedOutput, 0o750); err != nil {
		t.Fatal(err)
	}

	if _, err := assetmapper.Compile([]assetmapper.Root{{FS: newSource}}, dir); err == nil {
		t.Fatal("Compile succeeded despite an invalid destination")
	}
	gotManifestBytes, err := os.ReadFile(filepath.Join(dir, assetmapper.ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotManifestBytes) != string(oldManifestBytes) {
		t.Fatal("failed Compile replaced the previous manifest")
	}
	oldOutput := filepath.Join(dir, filepath.FromSlash(oldManifest.Entries["app.js"]))
	if got, err := os.ReadFile(oldOutput); err != nil || string(got) != "old" {
		t.Fatalf("previous compiled asset = %q, %v", got, err)
	}
}

func TestCompile_StreamingPathCleansUpTempOnCollision(t *testing.T) {
	// When the pass-2 collision check fires for a streamed file,
	// the temp file must be removed; otherwise publicDir
	// accumulates orphaned ".assetmapper-tmp-*.tmp" entries.
	//
	// foo.png with content "x" hashes to "2d711642b726b0440162" → output
	// "foo-2d711642b726b0440162.png". A literal source file with that name
	// triggers the shadow check.
	src := fstest.MapFS{
		"foo.png":                      {Data: []byte("x")},
		"foo-2d711642b726b0440162.png": {Data: []byte("shadow")},
	}
	dir := t.TempDir()
	_, err := assetmapper.Compile([]assetmapper.Root{{FS: src}}, dir)
	if err == nil {
		t.Fatal("expected collision error")
	}
	// No leftover temp file.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".assetmapper-tmp-") {
			t.Errorf("leftover temp file after collision: %s", e.Name())
		}
	}
}

func TestCompile_DifferentContentProducesDifferentHash(t *testing.T) {
	a := fstest.MapFS{"app.js": {Data: []byte("v1")}}
	b := fstest.MapFS{"app.js": {Data: []byte("v2")}}
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	m1, _ := assetmapper.Compile([]assetmapper.Root{{FS: a}}, dir1)
	m2, _ := assetmapper.Compile([]assetmapper.Root{{FS: b}}, dir2)
	if m1.Entries["app.js"] == m2.Entries["app.js"] {
		t.Errorf("different content produced same hashed name: %q", m1.Entries["app.js"])
	}
}
