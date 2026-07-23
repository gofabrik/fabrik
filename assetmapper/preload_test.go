package assetmapper_test

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gofabrik/fabrik/assetmapper"
)

// --- ModulePreloadLinks ---

func TestModulePreloadLinks_NoEntrypoints(t *testing.T) {
	src := fstest.MapFS{"app.js": {Data: []byte("export default {}")}}
	m := newRenderMapper(t, src)
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}

	got, err := im.ModulePreloadLinks(m)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("ModulePreloadLinks() = %q, want empty (no entrypoints requested)", got)
	}
}

func TestModulePreloadLinks_SingleEntrypointNoDeps(t *testing.T) {
	src := fstest.MapFS{"app.js": {Data: []byte("export default {}")}}
	m := newRenderMapper(t, src)
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}

	got, err := im.ModulePreloadLinks(m, "app")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `<link rel="modulepreload" href="/assets/app-`) {
		t.Errorf("missing modulepreload for entrypoint; got:\n%s", got)
	}
	// Entrypoint with no deps emits one link.
	if n := strings.Count(got, "<link"); n != 1 {
		t.Errorf("link count = %d, want 1; got:\n%s", n, got)
	}
}

func TestModulePreloadLinks_WalksRelativeImports(t *testing.T) {
	src := fstest.MapFS{
		"app.js":  {Data: []byte(`import u from "./util.js"; u();`)},
		"util.js": {Data: []byte(`export default function(){}`)},
	}
	m := newRenderMapper(t, src)
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}

	got, err := im.ModulePreloadLinks(m, "app")
	if err != nil {
		t.Fatal(err)
	}
	// Both app and util should appear.
	if !strings.Contains(got, "/assets/app-") {
		t.Errorf("missing entrypoint preload; got:\n%s", got)
	}
	if !strings.Contains(got, "/assets/util-") {
		t.Errorf("missing transitive dep preload; got:\n%s", got)
	}
	if n := strings.Count(got, "<link"); n != 2 {
		t.Errorf("link count = %d, want 2; got:\n%s", n, got)
	}
}

func TestModulePreloadLinks_DiamondDeps_NoDuplicates(t *testing.T) {
	// app imports a and b; both a and b import util. Util must
	// appear exactly once in the preload list.
	src := fstest.MapFS{
		"app.js":  {Data: []byte(`import a from "./a.js"; import b from "./b.js";`)},
		"a.js":    {Data: []byte(`import u from "./util.js"; export default u;`)},
		"b.js":    {Data: []byte(`import u from "./util.js"; export default u;`)},
		"util.js": {Data: []byte(`export default {}`)},
	}
	m := newRenderMapper(t, src)
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}

	got, err := im.ModulePreloadLinks(m, "app")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(got, "<link"); n != 4 {
		t.Errorf("link count = %d, want 4 (app + a + b + util once); got:\n%s", n, got)
	}
	if c := strings.Count(got, "/assets/util-"); c != 1 {
		t.Errorf("util preload count = %d, want 1; got:\n%s", c, got)
	}
}

func TestModulePreloadLinks_CyclicImports_NoInfiniteLoop(t *testing.T) {
	// Compile rejects cycles, but preload walking still terminates.
	src := fstest.MapFS{
		"a.js": {Data: []byte(`import b from "./b.js"; export default b;`)},
		"b.js": {Data: []byte(`import a from "./a.js"; export default a;`)},
	}
	m := newRenderMapper(t, src)
	im := assetmapper.NewImportmap()
	im.Entries["a"] = assetmapper.ImportmapEntry{Path: "a.js", Entrypoint: true}

	got, err := im.ModulePreloadLinks(m, "a")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(got, "<link"); n != 2 {
		t.Errorf("link count = %d, want 2 (a + b, each once)", n)
	}
}

func TestModulePreloadLinks_BareSpecifierResolvedViaImportmap(t *testing.T) {
	// app imports "react" (bare). Walker must consult the importmap
	// and preload the local vendor file.
	src := fstest.MapFS{
		"app.js":          {Data: []byte(`import React from "react"; export default React;`)},
		"vendor/react.js": {Data: []byte(`export default {createElement(){}}`)},
	}
	m := newRenderMapper(t, src)
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}
	im.Entries["react"] = assetmapper.ImportmapEntry{Version: "18.2.0"}

	got, err := im.ModulePreloadLinks(m, "app")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "/assets/vendor/react-") {
		t.Errorf("vendored react not preloaded; got:\n%s", got)
	}
}

func TestModulePreloadLinks_CSSEntrypointEmitsNoModulePreload(t *testing.T) {
	// CSS entrypoints fetch via <link rel="stylesheet">; modulepreload
	// is JS-only.
	src := fstest.MapFS{"styles/main.css": {Data: []byte("body{}")}}
	m := newRenderMapper(t, src)
	im := assetmapper.NewImportmap()
	im.Entries["styles"] = assetmapper.ImportmapEntry{
		Path: "styles/main.css", Type: "css", Entrypoint: true,
	}

	got, err := im.ModulePreloadLinks(m, "styles")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("ModulePreloadLinks for CSS entrypoint = %q, want empty", got)
	}
}

func TestModulePreloadLinks_MixedJSAndCSSEntrypoints(t *testing.T) {
	// JS entrypoint contributes modulepreload, CSS entrypoint
	// contributes nothing.
	src := fstest.MapFS{
		"app.js":          {Data: []byte("export default {}")},
		"styles/main.css": {Data: []byte("body{}")},
	}
	m := newRenderMapper(t, src)
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}
	im.Entries["styles"] = assetmapper.ImportmapEntry{
		Path: "styles/main.css", Type: "css", Entrypoint: true,
	}

	got, err := im.ModulePreloadLinks(m, "app", "styles")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "/assets/app-") {
		t.Errorf("JS entrypoint not preloaded; got:\n%s", got)
	}
	if strings.Contains(got, "/assets/styles/main-") {
		t.Errorf("CSS entrypoint should not appear in modulepreload; got:\n%s", got)
	}
}

func TestModulePreloadLinks_RejectsUnknownEntrypoint(t *testing.T) {
	im := assetmapper.NewImportmap()
	m := newRenderMapper(t, fstest.MapFS{})
	if _, err := im.ModulePreloadLinks(m, "nope"); err == nil {
		t.Fatal("expected error for unknown entrypoint")
	}
}

func TestModulePreloadLinks_RejectsNonEntrypoint(t *testing.T) {
	src := fstest.MapFS{"vendor/react.js": {Data: []byte("//react")}}
	m := newRenderMapper(t, src)
	im := assetmapper.NewImportmap()
	im.Entries["react"] = assetmapper.ImportmapEntry{Version: "18.2.0"}
	if _, err := im.ModulePreloadLinks(m, "react"); err == nil {
		t.Fatal("expected error for non-entrypoint name")
	}
}

func TestModulePreloadLinks_DeterministicAcrossCalls(t *testing.T) {
	src := fstest.MapFS{
		"app.js": {Data: []byte(`import a from "./a.js"; import b from "./b.js"; import c from "./c.js";`)},
		"a.js":   {Data: []byte(`export default 1`)},
		"b.js":   {Data: []byte(`export default 2`)},
		"c.js":   {Data: []byte(`export default 3`)},
	}
	m := newRenderMapper(t, src)
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}

	first, err := im.ModulePreloadLinks(m, "app")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		next, _ := im.ModulePreloadLinks(m, "app")
		if next != first {
			t.Fatalf("ModulePreloadLinks output changed across calls\nfirst:\n%s\nnext:\n%s", first, next)
		}
	}
}

// --- Render integration: preloads appear between importmap and entrypoint scripts ---

func TestRender_IncludesPreloadsBeforeEntrypointScript(t *testing.T) {
	src := fstest.MapFS{
		"app.js":  {Data: []byte(`import u from "./util.js";`)},
		"util.js": {Data: []byte(`export default {}`)},
	}
	m := newRenderMapper(t, src)
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}

	got, err := im.Render(m, "app")
	if err != nil {
		t.Fatal(err)
	}
	im1 := strings.Index(got, `<script type="importmap">`)
	preload := strings.Index(got, `<link rel="modulepreload"`)
	module := strings.Index(got, `<script type="module" src="`)
	if im1 < 0 || preload < 0 || module < 0 {
		t.Fatalf("missing one of importmap/preload/entrypoint:\n%s", got)
	}
	if im1 >= preload || preload >= module {
		t.Errorf("wrong order; importmap@%d, preload@%d, module@%d", im1, preload, module)
	}
}

func TestRender_PreloadsTransitiveDeps(t *testing.T) {
	// Render preloads the entrypoint and every reachable dependency.
	src := fstest.MapFS{
		"app.js":  {Data: []byte(`import u from "./util.js";`)},
		"util.js": {Data: []byte(`export default {}`)},
	}
	m := newRenderMapper(t, src)
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}

	got, err := im.Render(m, "app")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(got, `<link rel="modulepreload"`); n != 2 {
		t.Errorf("modulepreload count = %d, want 2 (app + util); got:\n%s", n, got)
	}
}

// --- CSS imported from JS ---

func TestRender_EmitsCSSPreloadFromJSImport(t *testing.T) {
	// CSS imported from JS gets a preload hint.
	src := fstest.MapFS{
		"app.js":     {Data: []byte(`import "./styles.css"; export default {}`)},
		"styles.css": {Data: []byte("body{margin:0}")},
	}
	m := newRenderMapper(t, src)
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}

	got, err := im.Render(m, "app")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `<link rel="preload" as="style" href="/assets/styles-`) {
		t.Errorf("missing CSS preload from JS import; got:\n%s", got)
	}
	// And the entrypoint JS itself still gets modulepreload.
	if !strings.Contains(got, `<link rel="modulepreload" href="/assets/app-`) {
		t.Errorf("missing JS modulepreload; got:\n%s", got)
	}
}

func TestModulePreloadLinks_StillExcludesCSSFromJSImport(t *testing.T) {
	// ModulePreloadLinks emits only JavaScript modulepreload tags.
	src := fstest.MapFS{
		"app.js":     {Data: []byte(`import "./styles.css"; export default {}`)},
		"styles.css": {Data: []byte("body{}")},
	}
	m := newRenderMapper(t, src)
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}

	got, err := im.ModulePreloadLinks(m, "app")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, `rel="preload"`) {
		t.Errorf("ModulePreloadLinks emitted a non-modulepreload tag; got:\n%s", got)
	}
	if strings.Contains(got, "styles") {
		t.Errorf("ModulePreloadLinks included a CSS file; got:\n%s", got)
	}
	// JS entrypoint preload still emitted.
	if !strings.Contains(got, `<link rel="modulepreload" href="/assets/app-`) {
		t.Errorf("missing JS modulepreload; got:\n%s", got)
	}
}

func TestRender_CSSPreloadCarriesNonce(t *testing.T) {
	// The new <link rel="preload" as="style"> tags must respect
	// CSP nonces alongside the rest of the output.
	src := fstest.MapFS{
		"app.js":     {Data: []byte(`import "./styles.css"; export default {}`)},
		"styles.css": {Data: []byte("body{}")},
	}
	m := newRenderMapper(t, src)
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}

	got, err := im.RenderWithOptions(m, assetmapper.RenderOptions{
		Entrypoints: []string{"app"},
		Nonce:       "xyz",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `<link rel="preload" as="style" href="/assets/styles-`) {
		t.Errorf("missing CSS preload; got:\n%s", got)
	}
	if !strings.Contains(got, `as="style" href="/assets/styles-`) {
		t.Errorf("CSS preload missing nonce or shape; got:\n%s", got)
	}
	// At least one nonce="xyz" must appear on the CSS preload.
	// Count check: importmap (1) + app modulepreload (1) + CSS preload (1) + module script (1) = 4
	if c := strings.Count(got, `nonce="xyz"`); c != 4 {
		t.Errorf("nonce count = %d, want 4; got:\n%s", c, got)
	}
}
