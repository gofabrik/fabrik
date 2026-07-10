package assetmapper_test

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gofabrik/fabrik/assetmapper"
)

func newMapper(t *testing.T, src fstest.MapFS) *assetmapper.Mapper {
	t.Helper()
	m, err := assetmapper.New(assetmapper.Config{
		Roots: []assetmapper.Root{{FS: src}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func execTemplate(t *testing.T, src string, m *assetmapper.Mapper, im *assetmapper.Importmap) (string, error) {
	t.Helper()
	tpl, err := template.New("t").Funcs(assetmapper.FuncMap(m, im)).Parse(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var buf bytes.Buffer
	err = tpl.Execute(&buf, nil)
	return buf.String(), err
}

// --- asset helper ---

func TestFuncMap_AssetReturnsURL(t *testing.T) {
	src := fstest.MapFS{"images/logo.png": {Data: []byte("PNG")}}
	m := newMapper(t, src)
	im := assetmapper.NewImportmap()

	out, err := execTemplate(t, `<img src="{{ asset "images/logo.png" }}">`, m, im)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `src="/assets/images/logo-`) {
		t.Errorf("missing asset URL; got:\n%s", out)
	}
	if !strings.Contains(out, `.png">`) {
		t.Errorf("URL missing extension; got:\n%s", out)
	}
}

func TestFuncMap_AssetMissingPropagatesError(t *testing.T) {
	m := newMapper(t, fstest.MapFS{})
	im := assetmapper.NewImportmap()
	_, err := execTemplate(t, `{{ asset "missing.js" }}`, m, im)
	if err == nil {
		t.Fatal("expected template execution error for missing asset")
	}
}

// --- importmap helper ---

func TestFuncMap_ImportmapRendersScriptAndPreloads(t *testing.T) {
	src := fstest.MapFS{
		"app.js":  {Data: []byte(`import u from "./util.js";`)},
		"util.js": {Data: []byte(`export default {}`)},
	}
	m := newMapper(t, src)
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}

	out, err := execTemplate(t, `<head>{{ importmap "app" }}</head>`, m, im)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`<script type="importmap">`,
		`<link rel="modulepreload" href="/assets/app-`,
		`<link rel="modulepreload" href="/assets/util-`,
		`<script type="module">import "app";</script>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestFuncMap_ImportmapOutputIsNotEscaped(t *testing.T) {
	// importmap returns template.HTML so html/template preserves tags.
	src := fstest.MapFS{"app.js": {Data: []byte("export default {}")}}
	m := newMapper(t, src)
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}

	out, err := execTemplate(t, `{{ importmap "app" }}`, m, im)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "&lt;script") {
		t.Errorf("importmap output was escaped (should be template.HTML); got:\n%s", out)
	}
}

func TestFuncMap_ImportmapUnknownEntrypointPropagatesError(t *testing.T) {
	src := fstest.MapFS{"app.js": {Data: []byte("x")}}
	m := newMapper(t, src)
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}

	_, err := execTemplate(t, `{{ importmap "typo" }}`, m, im)
	if err == nil {
		t.Fatal("expected execution error for unknown entrypoint")
	}
}

// --- module_preload_links helper ---

func TestFuncMap_ModulePreloadLinksEmitsTags(t *testing.T) {
	src := fstest.MapFS{
		"app.js":  {Data: []byte(`import u from "./util.js";`)},
		"util.js": {Data: []byte(`export default {}`)},
	}
	m := newMapper(t, src)
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}

	out, err := execTemplate(t, `{{ module_preload_links "app" }}`, m, im)
	if err != nil {
		t.Fatal(err)
	}
	if c := strings.Count(out, `<link rel="modulepreload"`); c != 2 {
		t.Errorf("preload count = %d, want 2 (app + util); got:\n%s", c, out)
	}
}

// --- end-to-end ---

func TestFuncMap_EndToEndPage(t *testing.T) {
	// One template using all three helpers in their canonical
	// positions. Demonstrates the dev/prod-agnostic usage pattern.
	src := fstest.MapFS{
		"app.js":          {Data: []byte(`import u from "./util.js";`)},
		"util.js":         {Data: []byte(`export default {}`)},
		"images/logo.png": {Data: []byte("PNG")},
		"styles/main.css": {Data: []byte("body{margin:0}")},
	}
	m := newMapper(t, src)
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}
	im.Entries["styles"] = assetmapper.ImportmapEntry{
		Path: "styles/main.css", Type: "css", Entrypoint: true,
	}

	page := `<!doctype html>
<html><head>
{{ importmap "app" "styles" }}
</head><body>
<img src="{{ asset "images/logo.png" }}">
</body></html>`

	out, err := execTemplate(t, page, m, im)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`<script type="importmap">`,
		`<link rel="modulepreload" href="/assets/app-`,
		`<link rel="stylesheet" href="/assets/styles/main-`,
		`<script type="module">import "app";</script>`,
		`<img src="/assets/images/logo-`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestFuncMap_DevToProdSwap(t *testing.T) {
	// Same template, two different mappers: one dev (no manifest),
	// one prod (manifest). Templates do not change; the output URLs
	// reflect the active mode.
	src := fstest.MapFS{"app.js": {Data: []byte("x")}}

	dev := newMapper(t, src)
	manifest := &assetmapper.Manifest{Entries: map[string]string{
		"app.js": "app-aaaaaaaa.js",
	}}
	prod, err := assetmapper.New(assetmapper.Config{
		Roots:    []assetmapper.Root{{FS: src}},
		Manifest: manifest,
	})
	if err != nil {
		t.Fatal(err)
	}

	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}

	page := `{{ asset "app.js" }}`
	devOut, err := execTemplate(t, page, dev, im)
	if err != nil {
		t.Fatal(err)
	}
	prodOut, err := execTemplate(t, page, prod, im)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(devOut, "/assets/app-") {
		t.Errorf("dev URL = %q, want /assets/app-<hash>.js", devOut)
	}
	if prodOut != "/assets/app-aaaaaaaa.js" {
		t.Errorf("prod URL = %q, want /assets/app-aaaaaaaa.js (from manifest)", prodOut)
	}
}

// --- CSP nonce helpers ---

func TestFuncMap_ImportmapNonceHelper(t *testing.T) {
	src := fstest.MapFS{"app.js": {Data: []byte("export default {}")}}
	m := newMapper(t, src)
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}

	out, err := execTemplate(t, `{{ importmap_nonce "xyz" "app" }}`, m, im)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `nonce="xyz"`) {
		t.Errorf("missing nonce attribute; got:\n%s", out)
	}
	if !strings.Contains(out, `<script type="importmap" nonce="xyz">`) {
		t.Errorf("importmap tag missing nonce; got:\n%s", out)
	}
}

func TestFuncMap_ModulePreloadLinksNonceHelper(t *testing.T) {
	src := fstest.MapFS{
		"app.js":  {Data: []byte(`import u from "./util.js";`)},
		"util.js": {Data: []byte("export default {}")},
	}
	m := newMapper(t, src)
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}

	out, err := execTemplate(t, `{{ module_preload_links_nonce "xyz" "app" }}`, m, im)
	if err != nil {
		t.Fatal(err)
	}
	if c := strings.Count(out, `nonce="xyz"`); c != 2 {
		t.Errorf("nonce count = %d, want 2 (app + util); got:\n%s", c, out)
	}
}

func TestFuncMap_NonceHelperEmptyNonceMatchesPlain(t *testing.T) {
	// Passing "" as the nonce should produce identical output to the
	// non-nonce helper.
	src := fstest.MapFS{"app.js": {Data: []byte("export default {}")}}
	m := newMapper(t, src)
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}

	plain, err := execTemplate(t, `{{ importmap "app" }}`, m, im)
	if err != nil {
		t.Fatal(err)
	}
	withEmpty, err := execTemplate(t, `{{ importmap_nonce "" "app" }}`, m, im)
	if err != nil {
		t.Fatal(err)
	}
	if plain != withEmpty {
		t.Errorf("empty-nonce output diverged:\nplain:\n%s\nwithEmpty:\n%s", plain, withEmpty)
	}
}
