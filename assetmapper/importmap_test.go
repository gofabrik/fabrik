package assetmapper_test

import (
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gofabrik/fabrik/assetmapper"
)

// --- load / parse / save round-trip ---

func TestImportmap_RoundTrip(t *testing.T) {
	want := assetmapper.NewImportmap()
	want.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}
	want.Entries["react"] = assetmapper.ImportmapEntry{Version: "18.2.0"}
	want.Entries["main"] = assetmapper.ImportmapEntry{Path: "styles/main.css", Type: "css", Entrypoint: true}

	path := filepath.Join(t.TempDir(), assetmapper.ImportmapFilename)
	if err := want.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := assetmapper.LoadImportmap(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != len(want.Entries) {
		t.Fatalf("len = %d, want %d", len(got.Entries), len(want.Entries))
	}
	for k, v := range want.Entries {
		if got.Entries[k] != v {
			t.Errorf("Entries[%q] = %+v, want %+v", k, got.Entries[k], v)
		}
	}
}

func TestParseImportmap_RejectsUnknownFields(t *testing.T) {
	// Typo in a field name should not be silently dropped.
	r := strings.NewReader(`{"app":{"path":"app.js","entrypiont":true}}`)
	if _, err := assetmapper.ParseImportmap(r); err == nil {
		t.Fatal("expected error for unknown field")
	}
}

// --- Render: importmap content ---

func newRenderMapper(t *testing.T, src fstest.MapFS) *assetmapper.Mapper {
	t.Helper()
	m, err := assetmapper.New(assetmapper.Config{
		Roots: []assetmapper.Root{{FS: src}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestImportmap_RendersImportmapWithoutEntrypoints(t *testing.T) {
	src := fstest.MapFS{"app.js": {Data: []byte("x")}}
	m := newRenderMapper(t, src)
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}

	html, err := im.Render(m)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(html, `<script type="importmap">`) {
		t.Errorf("output does not start with importmap script tag; got:\n%s", html)
	}
	if !strings.Contains(html, `"app":`) {
		t.Errorf("importmap missing app entry; got:\n%s", html)
	}
	if strings.Contains(html, "type=\"module\"") {
		t.Errorf("output should NOT include entrypoint tag when none requested; got:\n%s", html)
	}
}

func TestImportmap_RendersJSEntrypoint(t *testing.T) {
	src := fstest.MapFS{"app.js": {Data: []byte("console.log('hi')")}}
	m := newRenderMapper(t, src)
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}

	html, err := im.Render(m, "app")
	if err != nil {
		t.Fatal(err)
	}
	// Entrypoint as a bare-specifier import resolved by the
	// importmap: <script type="module">import "app";</script>
	if !strings.Contains(html, `<script type="module">import "app";</script>`) {
		t.Errorf("missing JS entrypoint import; got:\n%s", html)
	}
}

func TestImportmap_RendersCSSEntrypoint(t *testing.T) {
	src := fstest.MapFS{"styles/main.css": {Data: []byte("body{}")}}
	m := newRenderMapper(t, src)
	im := assetmapper.NewImportmap()
	im.Entries["styles"] = assetmapper.ImportmapEntry{
		Path: "styles/main.css", Type: "css", Entrypoint: true,
	}

	html, err := im.Render(m, "styles")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, `<link rel="stylesheet" href="/assets/styles/main-`) {
		t.Errorf("missing CSS entrypoint link; got:\n%s", html)
	}
}

func TestImportmap_RendersMultipleEntrypoints(t *testing.T) {
	src := fstest.MapFS{
		"app.js":          {Data: []byte("a")},
		"styles/main.css": {Data: []byte("body{}")},
	}
	m := newRenderMapper(t, src)
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}
	im.Entries["styles"] = assetmapper.ImportmapEntry{
		Path: "styles/main.css", Type: "css", Entrypoint: true,
	}

	html, err := im.Render(m, "app", "styles")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, `import "app";`) {
		t.Errorf("missing JS entrypoint; got:\n%s", html)
	}
	if !strings.Contains(html, `<link rel="stylesheet"`) {
		t.Errorf("missing CSS entrypoint; got:\n%s", html)
	}
}

func TestImportmap_RejectsUnknownEntrypointName(t *testing.T) {
	im := assetmapper.NewImportmap()
	m := newRenderMapper(t, fstest.MapFS{})
	if _, err := im.Render(m, "nope"); err == nil {
		t.Fatal("expected error for unknown entrypoint name")
	}
}

func TestImportmap_RejectsNonEntrypointName(t *testing.T) {
	// Importable modules are not automatically page entrypoints.
	src := fstest.MapFS{"vendor/react.js": {Data: []byte("//react")}}
	m := newRenderMapper(t, src)
	im := assetmapper.NewImportmap()
	im.Entries["react"] = assetmapper.ImportmapEntry{Version: "18.2.0"}

	_, err := im.Render(m, "react")
	if err == nil {
		t.Fatal("expected error for non-entrypoint name")
	}
	if !strings.Contains(err.Error(), "not marked as entrypoint") {
		t.Errorf("error message = %q, want it to mention non-entrypoint status", err)
	}
}

func TestImportmap_RejectsEntryWithBothPathAndVersion(t *testing.T) {
	src := fstest.MapFS{"app.js": {Data: []byte("x")}}
	m := newRenderMapper(t, src)
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{
		Path:    "app.js",
		Version: "1.0.0",
	}
	if _, err := im.Render(m); err == nil {
		t.Fatal("expected error for ambiguous entry (both path and version)")
	}
}

func TestImportmap_RejectsEntryWithNeitherPathNorVersion(t *testing.T) {
	im := assetmapper.NewImportmap()
	im.Entries["empty"] = assetmapper.ImportmapEntry{}
	m := newRenderMapper(t, fstest.MapFS{})
	if _, err := im.Render(m); err == nil {
		t.Fatal("expected error for empty entry")
	}
}

// --- Vendored convention path ---

func TestImportmap_VendoredEntryResolvesViaVendorPath(t *testing.T) {
	// "react" with Version set should resolve to vendor/react.js.
	src := fstest.MapFS{
		"vendor/react.js": {Data: []byte("//react")},
	}
	m := newRenderMapper(t, src)
	im := assetmapper.NewImportmap()
	im.Entries["react"] = assetmapper.ImportmapEntry{Version: "18.2.0"}

	html, err := im.Render(m)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, `"react":"/assets/vendor/react-`) {
		t.Errorf("vendored entry not resolved via vendor/<key>.js convention; got:\n%s", html)
	}
}

func TestImportmap_VendoredCSSResolvesToVendorCSSPath(t *testing.T) {
	src := fstest.MapFS{"vendor/normalize.css": {Data: []byte("*{}")}}
	m := newRenderMapper(t, src)
	im := assetmapper.NewImportmap()
	im.Entries["normalize"] = assetmapper.ImportmapEntry{
		Version: "8.0.1", Type: "css",
	}

	html, err := im.Render(m)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, `"normalize":"/assets/vendor/normalize-`) {
		t.Errorf("vendored CSS not resolved via vendor/<key>.css convention; got:\n%s", html)
	}
}

// --- Output stability ---

func TestImportmap_RenderIsKeySorted(t *testing.T) {
	// Map iteration is randomised; rendered output must not be. The
	// browser doesn't care, but operators reading the page source
	// (and diff tools comparing generated HTML) do.
	src := fstest.MapFS{
		"a.js": {Data: []byte("a")},
		"b.js": {Data: []byte("b")},
		"c.js": {Data: []byte("c")},
	}
	m := newRenderMapper(t, src)
	im := assetmapper.NewImportmap()
	im.Entries["zebra"] = assetmapper.ImportmapEntry{Path: "c.js"}
	im.Entries["apple"] = assetmapper.ImportmapEntry{Path: "a.js"}
	im.Entries["mango"] = assetmapper.ImportmapEntry{Path: "b.js"}

	first, err := im.Render(m)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		next, err := im.Render(m)
		if err != nil {
			t.Fatal(err)
		}
		if next != first {
			t.Fatalf("rendered output changed across calls (map iteration order leaked)\nfirst:\n%s\nnext:\n%s", first, next)
		}
	}
	// Apple < mango < zebra by ASCII order.
	apple := strings.Index(first, `"apple"`)
	mango := strings.Index(first, `"mango"`)
	zebra := strings.Index(first, `"zebra"`)
	if apple >= mango || mango >= zebra {
		t.Errorf("keys not sorted; positions: apple=%d mango=%d zebra=%d\noutput:\n%s",
			apple, mango, zebra, first)
	}
}

func TestImportmap_RejectsNilMapper(t *testing.T) {
	im := assetmapper.NewImportmap()
	if _, err := im.Render(nil); err == nil {
		t.Fatal("expected error for nil mapper")
	}
}

// --- CSP nonce support ---

func TestImportmap_RenderWithOptions_AddsNonceToAllTags(t *testing.T) {
	// Every emitted tag carries the supplied nonce.
	src := fstest.MapFS{
		"app.js":          {Data: []byte(`import u from "./util.js";`)},
		"util.js":         {Data: []byte(`export default {}`)},
		"styles/main.css": {Data: []byte("body{}")},
	}
	m := newRenderMapper(t, src)
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}
	im.Entries["styles"] = assetmapper.ImportmapEntry{
		Path: "styles/main.css", Type: "css", Entrypoint: true,
	}

	html, err := im.RenderWithOptions(m, assetmapper.RenderOptions{
		Entrypoints: []string{"app", "styles"},
		Nonce:       "abc123",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Every <script and <link tag should have nonce="abc123".
	for _, want := range []string{
		`<script type="importmap" nonce="abc123">`,
		`<link rel="modulepreload" href="/assets/app-`,
		` nonce="abc123">`,
		`<link rel="stylesheet" href="/assets/styles/main-`,
		`<script type="module" nonce="abc123">import "app";</script>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q in:\n%s", want, html)
		}
	}
	// And a count check: nonce should appear once per emitted tag.
	// importmap (1) + 2 modulepreloads (2) + stylesheet (1) + module (1) = 5
	if got := strings.Count(html, `nonce="abc123"`); got != 5 {
		t.Errorf("nonce count = %d, want 5; output:\n%s", got, html)
	}
}

func TestImportmap_RenderWithOptions_EmptyNonceMatchesPlainRender(t *testing.T) {
	// Empty nonce preserves the plain Render output.
	src := fstest.MapFS{
		"app.js":  {Data: []byte(`import u from "./util.js";`)},
		"util.js": {Data: []byte(`export default {}`)},
	}
	m := newRenderMapper(t, src)
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}

	plain, err := im.Render(m, "app")
	if err != nil {
		t.Fatal(err)
	}
	withOpts, err := im.RenderWithOptions(m, assetmapper.RenderOptions{
		Entrypoints: []string{"app"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plain != withOpts {
		t.Errorf("output diverged:\nplain:\n%s\nwithOpts:\n%s", plain, withOpts)
	}
	if strings.Contains(plain, "nonce=") {
		t.Errorf("empty nonce should NOT add the attribute; got:\n%s", plain)
	}
}

func TestImportmap_RenderWithOptions_NonceIsHTMLEscaped(t *testing.T) {
	// Escape nonce values defensively; callers may pass invalid CSP tokens.
	src := fstest.MapFS{"app.js": {Data: []byte("export default {}")}}
	m := newRenderMapper(t, src)
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}

	html, err := im.RenderWithOptions(m, assetmapper.RenderOptions{
		Entrypoints: []string{"app"},
		Nonce:       `"><script>alert(1)</script>`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(html, `<script>alert(1)`) {
		t.Errorf("nonce was not escaped; injection possible:\n%s", html)
	}
	// The escaped form should appear instead.
	if !strings.Contains(html, `&#34;&gt;&lt;script&gt;`) {
		t.Errorf("expected HTML-escaped nonce; got:\n%s", html)
	}
}

func TestImportmap_ModulePreloadLinksWithOptions_AddsNonce(t *testing.T) {
	src := fstest.MapFS{
		"app.js":  {Data: []byte(`import u from "./util.js";`)},
		"util.js": {Data: []byte(`export default {}`)},
	}
	m := newRenderMapper(t, src)
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}

	got, err := im.ModulePreloadLinksWithOptions(m, assetmapper.RenderOptions{
		Entrypoints: []string{"app"},
		Nonce:       "abc123",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, ` nonce="abc123">`) {
		t.Errorf("missing nonce attribute; got:\n%s", got)
	}
	// Two preloads (app + util) → two nonce occurrences.
	if c := strings.Count(got, `nonce="abc123"`); c != 2 {
		t.Errorf("nonce count = %d, want 2; got:\n%s", c, got)
	}
}

func TestImportmap_ModulePreloadLinksWithOptions_EmptyNonceMatchesPlain(t *testing.T) {
	src := fstest.MapFS{"app.js": {Data: []byte("export default {}")}}
	m := newRenderMapper(t, src)
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}

	plain, err := im.ModulePreloadLinks(m, "app")
	if err != nil {
		t.Fatal(err)
	}
	withOpts, err := im.ModulePreloadLinksWithOptions(m, assetmapper.RenderOptions{
		Entrypoints: []string{"app"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plain != withOpts {
		t.Errorf("output diverged:\nplain:\n%s\nwithOpts:\n%s", plain, withOpts)
	}
}
