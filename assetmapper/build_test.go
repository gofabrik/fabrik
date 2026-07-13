package assetmapper_test

import (
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gofabrik/fabrik/assetmapper"
)

func buildOrFatal(t *testing.T, roots []assetmapper.Root, im *assetmapper.Importmap, opts ...assetmapper.BuildOption) *assetmapper.Compiled {
	t.Helper()
	c, err := assetmapper.Build(roots, im, opts...)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return c
}

func get(t *testing.T, h http.Handler, url string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	return rec
}

func TestBuildRootErrorProvenance(t *testing.T) {
	good := fstest.MapFS{"style.css": {Data: []byte(`body{}`)}}

	// normalizeRoots: a nil FS at index 1 is attributable to that root.
	_, err := assetmapper.Build([]assetmapper.Root{{FS: good}, {FS: nil}}, assetmapper.NewImportmap())
	var re *assetmapper.RootError
	if !errors.As(err, &re) {
		t.Fatalf("nil FS: err = %v, want *RootError", err)
	}
	if re.Index != 1 {
		t.Fatalf("nil FS: RootError.Index = %d, want 1", re.Index)
	}

	// discoverImportmap: a malformed importmap.json in root 1 (nil im triggers discovery).
	bad := fstest.MapFS{"importmap.json": {Data: []byte(`{ not json`)}}
	re = nil
	_, err = assetmapper.Build([]assetmapper.Root{{FS: good}, {FS: bad}}, nil)
	if !errors.As(err, &re) {
		t.Fatalf("bad importmap: err = %v, want *RootError", err)
	}
	if re.Index != 1 {
		t.Fatalf("bad importmap: RootError.Index = %d, want 1", re.Index)
	}
}

func TestBuildResolvesAndServes(t *testing.T) {
	imageBytes := []byte("PNG-NOT-REALLY")
	shared := fstest.MapFS{
		"style.css":     {Data: []byte(`body { background: url("./images/bg.png"); }`)},
		"images/bg.png": {Data: imageBytes},
	}
	web := fstest.MapFS{
		"app.js": {Data: []byte(`import { nav } from "./nav.js"; nav();`)},
		"nav.js": {Data: []byte(`export function nav() {}`)},
	}
	c := buildOrFatal(t, []assetmapper.Root{{FS: shared}, {FS: web}}, nil)

	cssURL, err := c.Asset("style.css")
	if err != nil {
		t.Fatalf("Asset(style.css): %v", err)
	}
	if !strings.HasPrefix(cssURL, "/assets/style-") || !strings.HasSuffix(cssURL, ".css") {
		t.Fatalf("unexpected css URL %q", cssURL)
	}
	imgURL, err := c.Asset("images/bg.png")
	if err != nil {
		t.Fatalf("Asset(images/bg.png): %v", err)
	}

	h := c.Handler()

	rec := get(t, h, cssURL)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status %d", cssURL, rec.Code)
	}
	if !strings.Contains(rec.Body.String(), imgURL) {
		t.Fatalf("compiled CSS %q does not reference hashed image URL %q", rec.Body.String(), imgURL)
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if rec.Header().Get("ETag") == "" {
		t.Fatal("missing ETag")
	}

	rec = get(t, h, imgURL)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status %d", imgURL, rec.Code)
	}
	if rec.Body.String() != string(imageBytes) {
		t.Fatalf("image body = %q, want source bytes", rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("image Content-Type = %q", got)
	}

	jsURL, err := c.Asset("app.js")
	if err != nil {
		t.Fatalf("Asset(app.js): %v", err)
	}
	navURL, _ := c.Asset("nav.js")
	rec = get(t, h, jsURL)
	if !strings.Contains(rec.Body.String(), navURL) {
		t.Fatalf("compiled JS %q does not reference %q", rec.Body.String(), navURL)
	}
}

// Query strings and fragments address content inside the resolved asset.
func TestRewritePreservesQueryAndFragment(t *testing.T) {
	fsys := fstest.MapFS{
		"style.css": {Data: []byte(
			`@font-face { src: url("./font.woff2?#iefix"); } .i { background: url("./icons.svg#check"); }`)},
		"font.woff2": {Data: []byte("woff2")},
		"icons.svg":  {Data: []byte("<svg/>")},
		"app.js":     {Data: []byte(`import "./m.js?v=1";`)},
		"m.js":       {Data: []byte("export {}")},
	}
	c := buildOrFatal(t, []assetmapper.Root{{FS: fsys}}, nil)
	h := c.Handler()

	fontURL, _ := c.Asset("font.woff2")
	iconsURL, _ := c.Asset("icons.svg")
	mURL, _ := c.Asset("m.js")

	cssURL, _ := c.Asset("style.css")
	css := get(t, h, cssURL).Body.String()
	for _, want := range []string{fontURL + "?#iefix", iconsURL + "#check"} {
		if !strings.Contains(css, want) {
			t.Errorf("compiled CSS %q lacks %q", css, want)
		}
	}

	jsURL, _ := c.Asset("app.js")
	js := get(t, h, jsURL).Body.String()
	if want := mURL + "?v=1"; !strings.Contains(js, want) {
		t.Errorf("compiled JS %q lacks %q", js, want)
	}
}

func TestHandlerMethodsAndMisses(t *testing.T) {
	c := buildOrFatal(t, []assetmapper.Root{{FS: fstest.MapFS{"app.css": {Data: []byte("body{}")}}}}, nil)
	h := c.Handler()
	url, _ := c.Asset("app.css")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, url, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
		t.Fatalf("Allow = %q", got)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, url, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD wrote %d body bytes", rec.Body.Len())
	}
	if rec.Header().Get("Content-Length") == "" {
		t.Fatal("HEAD missing Content-Length")
	}

	etag := get(t, h, url).Header().Get("ETag")
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("If-None-Match", etag)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match status = %d, want 304", rec.Code)
	}

	for _, miss := range []string{"/assets/", "/assets/app.css", "/assets/app-00000000.css", "/other/app.css"} {
		if code := get(t, h, miss).Code; code != http.StatusNotFound {
			t.Fatalf("GET %s: status %d, want 404", miss, code)
		}
	}
}

func TestBuildWithURLPrefix(t *testing.T) {
	c := buildOrFatal(t, []assetmapper.Root{{FS: fstest.MapFS{"app.css": {Data: []byte("body{}")}}}}, nil,
		assetmapper.WithURLPrefix("/static/"))
	url, err := c.Asset("app.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(url, "/static/") {
		t.Fatalf("URL %q not under /static/", url)
	}
	if got := c.URLPrefix(); got != "/static/" {
		t.Fatalf("URLPrefix() = %q", got)
	}
	if code := get(t, c.Handler(), url).Code; code != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, code)
	}
}

func TestBuildDiscoversImportmap(t *testing.T) {
	root := fstest.MapFS{
		"assets/app.js": {Data: []byte("export {}")},
		"assets/importmap.json": {Data: []byte(`{
			"app": {"path": "app.js", "entrypoint": true}
		}`)},
	}
	c := buildOrFatal(t, []assetmapper.Root{{FS: root, Dir: "assets"}}, nil)

	fn, ok := c.FuncMap()["importmap"].(func(...string) (template.HTML, error))
	if !ok {
		t.Fatal("importmap helper has unexpected type")
	}
	html, err := fn("app")
	if err != nil {
		t.Fatalf("importmap render: %v", err)
	}
	appURL, _ := c.Asset("app.js")
	if !strings.Contains(string(html), appURL) {
		t.Fatalf("importmap output %q lacks %q", html, appURL)
	}

	if _, err := c.Asset("importmap.json"); !errors.Is(err, assetmapper.ErrAssetNotFound) {
		t.Fatalf("Asset(importmap.json) err = %v, want ErrAssetNotFound", err)
	}
}

// Top-level importmap.json is configuration in both dev and build modes.
func TestDevMapperExcludesImportmap(t *testing.T) {
	fsys := fstest.MapFS{
		"app.js":              {Data: []byte("export {}")},
		"importmap.json":      {Data: []byte(`{"app": {"path": "app.js"}}`)},
		"deep/importmap.json": {Data: []byte("an ordinary asset, not configuration")},
	}
	m, err := assetmapper.New(assetmapper.Config{Roots: []assetmapper.Root{{FS: fsys}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Asset("importmap.json"); !errors.Is(err, assetmapper.ErrAssetNotFound) {
		t.Fatalf("dev Asset(importmap.json) err = %v, want ErrAssetNotFound", err)
	}
	if _, err := m.Asset("deep/importmap.json"); err != nil {
		t.Fatalf("nested importmap.json is an ordinary asset: %v", err)
	}
}

func TestBuildImportmapErrors(t *testing.T) {
	appFS := fstest.MapFS{"app.js": {Data: []byte("export {}")}}
	imFS := func(body string) fstest.MapFS {
		return fstest.MapFS{
			"app.js":         {Data: []byte("export {}")},
			"importmap.json": {Data: []byte(body)},
		}
	}
	entry := `{"app": {"path": "app.js"}}`

	cases := []struct {
		name  string
		roots []assetmapper.Root
		im    *assetmapper.Importmap
		want  string
	}{
		{
			name:  "two roots carry importmap.json",
			roots: []assetmapper.Root{{FS: imFS(entry)}, {FS: imFS(entry)}},
			want:  "only one root may carry it",
		},
		{
			name:  "malformed importmap.json",
			roots: []assetmapper.Root{{FS: imFS(`{"app": {`)}},
			want:  "ParseImportmap",
		},
		{
			name:  "entry names missing asset",
			roots: []assetmapper.Root{{FS: appFS}},
			im:    &assetmapper.Importmap{Entries: map[string]assetmapper.ImportmapEntry{"gone": {Path: "gone.js"}}},
			want:  "not a known asset",
		},
		{
			name:  "entry with both path and version",
			roots: []assetmapper.Root{{FS: appFS}},
			im:    &assetmapper.Importmap{Entries: map[string]assetmapper.ImportmapEntry{"app": {Path: "app.js", Version: "1.0.0"}}},
			want:  `both "path" and "version"`,
		},
		{
			name:  "entry with neither path nor version",
			roots: []assetmapper.Root{{FS: appFS}},
			im:    &assetmapper.Importmap{Entries: map[string]assetmapper.ImportmapEntry{"app": {}}},
			want:  `neither "path"`,
		},
		{
			name:  "entry with invalid type",
			roots: []assetmapper.Root{{FS: appFS}},
			im:    &assetmapper.Importmap{Entries: map[string]assetmapper.ImportmapEntry{"app": {Path: "app.js", Type: "wasm"}}},
			want:  `invalid type "wasm"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := assetmapper.Build(tc.roots, tc.im)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Build err = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestRootDirValidationAndMount(t *testing.T) {
	fsys := fstest.MapFS{"assets/app.css": {Data: []byte("body{}")}}

	for _, dir := range []string{"/assets", "assets/", "../assets", "./assets"} {
		if _, err := assetmapper.Build([]assetmapper.Root{{FS: fsys, Dir: dir}}, nil); err == nil {
			t.Fatalf("Build with Dir %q: want error", dir)
		}
	}

	c := buildOrFatal(t, []assetmapper.Root{{FS: fsys, Dir: "assets", MountAt: "shared"}}, nil)
	url, err := c.Asset("shared/app.css")
	if err != nil {
		t.Fatalf("Asset(shared/app.css): %v", err)
	}
	if !strings.HasPrefix(url, "/assets/shared/app-") {
		t.Fatalf("mounted URL = %q", url)
	}
}

func TestBuildCycleError(t *testing.T) {
	fsys := fstest.MapFS{
		"a.js": {Data: []byte(`import "./b.js";`)},
		"b.js": {Data: []byte(`import "./a.js";`)},
	}
	_, err := assetmapper.Build([]assetmapper.Root{{FS: fsys}}, nil)
	var ce *assetmapper.CycleError
	if !errors.As(err, &ce) {
		t.Fatalf("Build err = %v, want *CycleError", err)
	}
}

func TestBuildShadowingIsAFeature(t *testing.T) {
	first := fstest.MapFS{"app.css": {Data: []byte("body{color:red}")}}
	second := fstest.MapFS{"app.css": {Data: []byte("body{color:blue}")}}
	c := buildOrFatal(t, []assetmapper.Root{{FS: first}, {FS: second}}, nil)
	url, err := c.Asset("app.css")
	if err != nil {
		t.Fatal(err)
	}
	body := get(t, c.Handler(), url).Body.String()
	if body != "body{color:red}" {
		t.Fatalf("shadowing broke: body = %q, want first root's content", body)
	}
}

func TestCheckMirrorsBuild(t *testing.T) {
	good := []assetmapper.Root{{FS: fstest.MapFS{"app.css": {Data: []byte("body{}")}}}}
	if err := assetmapper.Check(good, nil); err != nil {
		t.Fatalf("Check on valid roots: %v", err)
	}
	if err := assetmapper.Check(good, nil, assetmapper.WithURLPrefix("no-slash")); err == nil {
		t.Fatal("Check with invalid prefix option: want error")
	}

	cyclic := []assetmapper.Root{{FS: fstest.MapFS{
		"a.js": {Data: []byte(`import "./b.js";`)},
		"b.js": {Data: []byte(`import "./a.js";`)},
	}}}
	cerr := assetmapper.Check(cyclic, nil)
	_, berr := assetmapper.Build(cyclic, nil)
	if cerr == nil || berr == nil || cerr.Error() != berr.Error() {
		t.Fatalf("Check and Build disagree:\n  Check: %v\n  Build: %v", cerr, berr)
	}
}

func TestBuildStreamsLargePassthrough(t *testing.T) {
	// A handler round trip for a multi-chunk file exercises the
	// io.Copy path end to end.
	big := strings.Repeat("x", 1<<16)
	c := buildOrFatal(t, []assetmapper.Root{{FS: fstest.MapFS{"data.bin": {Data: []byte(big)}}}}, nil)
	url, _ := c.Asset("data.bin")
	rec := get(t, c.Handler(), url)
	if body, _ := io.ReadAll(rec.Body); string(body) != big {
		t.Fatalf("streamed body mismatch: got %d bytes", len(body))
	}
	if got := rec.Header().Get("Content-Length"); got != fmt.Sprint(len(big)) {
		t.Fatalf("Content-Length = %q, want %d", got, len(big))
	}
}
