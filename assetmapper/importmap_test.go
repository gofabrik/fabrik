package assetmapper_test

import (
	"html/template"
	"path/filepath"
	"regexp"
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

var inlineScriptRE = regexp.MustCompile(`(?s)<script[^>]*>(.*?)</script>`)

func TestRenderParts_InlineScriptsMatchHTML(t *testing.T) {
	src := fstest.MapFS{
		"app.js": {Data: []byte(`console.log("app");`)},
		"lib.js": {Data: []byte(`console.log("lib");`)},
	}
	m, err := assetmapper.New(assetmapper.Config{Roots: []assetmapper.Root{{FS: src}}})
	if err != nil {
		t.Fatal(err)
	}
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}
	im.Entries["lib"] = assetmapper.ImportmapEntry{Path: "lib.js", Entrypoint: true}

	parts, err := im.RenderParts(m, assetmapper.RenderOptions{Entrypoints: []string{"app", "lib"}})
	if err != nil {
		t.Fatal(err)
	}
	var bodies []string
	for _, match := range inlineScriptRE.FindAllStringSubmatch(parts.HTML, -1) {
		bodies = append(bodies, match[1])
	}
	if len(bodies) != 3 {
		t.Fatalf("inline scripts in HTML = %d, want 3:\n%s", len(bodies), parts.HTML)
	}
	if len(parts.InlineScripts) != len(bodies) {
		t.Fatalf("InlineScripts = %d entries, want %d", len(parts.InlineScripts), len(bodies))
	}
	for i := range bodies {
		if parts.InlineScripts[i] != bodies[i] {
			t.Fatalf("InlineScripts[%d] = %q, HTML body = %q", i, parts.InlineScripts[i], bodies[i])
		}
	}
}

func TestRenderParts_CSSEntrypointAddsNoScript(t *testing.T) {
	src := fstest.MapFS{"main.css": {Data: []byte("body{}")}}
	m, err := assetmapper.New(assetmapper.Config{Roots: []assetmapper.Root{{FS: src}}})
	if err != nil {
		t.Fatal(err)
	}
	im := assetmapper.NewImportmap()
	im.Entries["main"] = assetmapper.ImportmapEntry{Path: "main.css", Type: "css", Entrypoint: true}
	parts, err := im.RenderParts(m, assetmapper.RenderOptions{Entrypoints: []string{"main"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(parts.InlineScripts) != 1 {
		t.Fatalf("InlineScripts = %d, want 1", len(parts.InlineScripts))
	}
}

func TestRenderParts_NonceLeavesBodiesUnchanged(t *testing.T) {
	src := fstest.MapFS{"app.js": {Data: []byte("x")}}
	m, err := assetmapper.New(assetmapper.Config{Roots: []assetmapper.Root{{FS: src}}})
	if err != nil {
		t.Fatal(err)
	}
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}

	plain, err := im.RenderParts(m, assetmapper.RenderOptions{Entrypoints: []string{"app"}})
	if err != nil {
		t.Fatal(err)
	}
	nonced, err := im.RenderParts(m, assetmapper.RenderOptions{Entrypoints: []string{"app"}, Nonce: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(nonced.HTML, `nonce="abc"`) {
		t.Fatal("nonce attribute missing")
	}
	if len(plain.InlineScripts) != len(nonced.InlineScripts) {
		t.Fatal("nonce changed the script count")
	}
	for i := range plain.InlineScripts {
		if plain.InlineScripts[i] != nonced.InlineScripts[i] {
			t.Fatalf("nonce changed body %d", i)
		}
	}
}

func TestRenderWithOptions_MatchesRenderPartsHTML(t *testing.T) {
	src := fstest.MapFS{"app.js": {Data: []byte("x")}}
	m, err := assetmapper.New(assetmapper.Config{Roots: []assetmapper.Root{{FS: src}}})
	if err != nil {
		t.Fatal(err)
	}
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}
	opts := assetmapper.RenderOptions{Entrypoints: []string{"app"}, Nonce: "n1"}
	html, err := im.RenderWithOptions(m, opts)
	if err != nil {
		t.Fatal(err)
	}
	parts, err := im.RenderParts(m, opts)
	if err != nil {
		t.Fatal(err)
	}
	if html != parts.HTML {
		t.Fatal("RenderWithOptions and RenderParts.HTML diverge")
	}
}

func TestFuncMapHelpers_MatchRenderParts(t *testing.T) {
	src := fstest.MapFS{"app.js": {Data: []byte("x")}}
	m, err := assetmapper.New(assetmapper.Config{Roots: []assetmapper.Root{{FS: src}}})
	if err != nil {
		t.Fatal(err)
	}
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}
	fm := assetmapper.FuncMap(m, im)

	plain, err := im.RenderParts(m, assetmapper.RenderOptions{Entrypoints: []string{"app"}})
	if err != nil {
		t.Fatal(err)
	}
	helper := fm["importmap"].(func(...string) (template.HTML, error))
	got, err := helper("app")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != plain.HTML {
		t.Fatal("importmap helper diverges from RenderParts.HTML")
	}

	nonced, err := im.RenderParts(m, assetmapper.RenderOptions{Entrypoints: []string{"app"}, Nonce: "n2"})
	if err != nil {
		t.Fatal(err)
	}
	nonceHelper := fm["importmap_nonce"].(func(string, ...string) (template.HTML, error))
	gotNonced, err := nonceHelper("n2", "app")
	if err != nil {
		t.Fatal(err)
	}
	if string(gotNonced) != nonced.HTML {
		t.Fatal("importmap_nonce helper diverges from RenderParts.HTML")
	}
}
