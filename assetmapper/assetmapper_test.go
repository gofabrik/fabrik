package assetmapper_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gofabrik/fabrik/assetmapper"
)

func mustMapper(t *testing.T, cfg assetmapper.Config) *assetmapper.Mapper {
	t.Helper()
	m, err := assetmapper.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// --- New / configuration ---

func TestNew_RejectsNoRoots(t *testing.T) {
	if _, err := assetmapper.New(assetmapper.Config{}); err == nil {
		t.Fatal("expected error for empty Roots")
	}
}

func TestNew_RejectsNilFS(t *testing.T) {
	_, err := assetmapper.New(assetmapper.Config{
		Roots: []assetmapper.Root{{FS: nil}},
	})
	if err == nil {
		t.Fatal("expected error for nil Roots[0].FS")
	}
}

func TestNew_RejectsBadMount(t *testing.T) {
	cases := []string{"/leading", "trailing/", "../traverse", "ok//slashes"}
	for _, mount := range cases {
		_, err := assetmapper.New(assetmapper.Config{
			Roots: []assetmapper.Root{{FS: fstest.MapFS{}, MountAt: mount}},
		})
		if err == nil {
			t.Errorf("MountAt=%q: expected error, got nil", mount)
		}
	}
}

func TestNew_DefaultsURLPrefix(t *testing.T) {
	m := mustMapper(t, assetmapper.Config{
		Roots: []assetmapper.Root{{FS: fstest.MapFS{}}},
	})
	if m.URLPrefix() != "/assets/" {
		t.Errorf("URLPrefix = %q, want /assets/", m.URLPrefix())
	}
}

func TestNew_NormalisesURLPrefix(t *testing.T) {
	m := mustMapper(t, assetmapper.Config{
		Roots:     []assetmapper.Root{{FS: fstest.MapFS{}}},
		URLPrefix: "/static",
	})
	if m.URLPrefix() != "/static/" {
		t.Errorf("URLPrefix = %q, want /static/ (trailing slash added)", m.URLPrefix())
	}
}

func TestNew_RejectsURLPrefixDriftFromManifest(t *testing.T) {
	// The whole point of persisting URLPrefix in the manifest:
	// Compile baked /assets/ into every rewritten reference. If New
	// is then handed the same manifest with Config.URLPrefix set to
	// /static/, Mapper.Asset would hand callers /static/app-HASH.js
	// while the file's content still references /assets/util-HASH.js.
	// Page half-loads silently. New must refuse at boot.
	src := fstest.MapFS{"app.js": {Data: []byte("export default {}")}}
	dir := t.TempDir()
	manifest, err := assetmapper.Compile([]assetmapper.Root{{FS: src}}, dir,
		assetmapper.WithURLPrefix("/assets/"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = assetmapper.New(assetmapper.Config{
		Roots:     []assetmapper.Root{{FS: src}},
		URLPrefix: "/static/",
		Manifest:  manifest,
	})
	if err == nil {
		t.Fatal("expected error for URLPrefix mismatch")
	}
	if !strings.Contains(err.Error(), "URLPrefix mismatch") {
		t.Errorf("error should mention 'URLPrefix mismatch'; got: %v", err)
	}
}

func TestNew_AcceptsMatchingURLPrefix(t *testing.T) {
	// Happy path: same prefix in both places → no error.
	src := fstest.MapFS{"app.js": {Data: []byte("x")}}
	dir := t.TempDir()
	manifest, err := assetmapper.Compile([]assetmapper.Root{{FS: src}}, dir,
		assetmapper.WithURLPrefix("/static/"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := assetmapper.New(assetmapper.Config{
		Roots:     []assetmapper.Root{{FS: src}},
		URLPrefix: "/static/",
		Manifest:  manifest,
	}); err != nil {
		t.Fatalf("matching prefix should pass: %v", err)
	}
}

func TestNew_AcceptsImplicitMatchingURLPrefix(t *testing.T) {
	// Default Config.URLPrefix ("") normalises to "/assets/", which
	// matches Compile's default. No error in the all-defaults case.
	src := fstest.MapFS{"app.js": {Data: []byte("x")}}
	dir := t.TempDir()
	manifest, err := assetmapper.Compile([]assetmapper.Root{{FS: src}}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := assetmapper.New(assetmapper.Config{
		Roots:    []assetmapper.Root{{FS: src}},
		Manifest: manifest,
	}); err != nil {
		t.Fatalf("default prefix in both places should pass: %v", err)
	}
}

func TestNew_LenientWhenManifestURLPrefixEmpty(t *testing.T) {
	// Hand-crafted Manifest fixtures (common in tests) don't set
	// URLPrefix. New treats an empty manifest URLPrefix as
	// "unspecified" and skips the check, matching the doc.
	src := fstest.MapFS{}
	manifest := &assetmapper.Manifest{Entries: map[string]string{
		"app.js": "app-deadbeef.js",
	}}
	if _, err := assetmapper.New(assetmapper.Config{
		Roots:     []assetmapper.Root{{FS: src}},
		URLPrefix: "/static/",
		Manifest:  manifest,
	}); err != nil {
		t.Fatalf("manifest without URLPrefix should not trigger drift check: %v", err)
	}
}

func TestCompile_PersistsURLPrefixInManifest(t *testing.T) {
	src := fstest.MapFS{"app.js": {Data: []byte("x")}}
	dir := t.TempDir()
	manifest, err := assetmapper.Compile([]assetmapper.Root{{FS: src}}, dir,
		assetmapper.WithURLPrefix("/static/v2/"))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.URLPrefix != "/static/v2/" {
		t.Errorf("manifest.URLPrefix = %q, want /static/v2/", manifest.URLPrefix)
	}
	// And it survives a disk round-trip.
	got, err := assetmapper.LoadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.URLPrefix != "/static/v2/" {
		t.Errorf("loaded manifest URLPrefix = %q, want /static/v2/", got.URLPrefix)
	}
}

func TestNew_CollapsesDoubleSlashesInURLPrefix(t *testing.T) {
	// URL prefixes are cleaned and keep a trailing slash.
	cases := []struct{ in, want string }{
		{"//static//", "/static/"},
		{"/a///b///", "/a/b/"},
		{"/single/", "/single/"},
	}
	for _, tc := range cases {
		m := mustMapper(t, assetmapper.Config{
			Roots:     []assetmapper.Root{{FS: fstest.MapFS{}}},
			URLPrefix: tc.in,
		})
		if got := m.URLPrefix(); got != tc.want {
			t.Errorf("URLPrefix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- Asset() resolution ---

func TestAsset_HashesContentInDevMode(t *testing.T) {
	fs := fstest.MapFS{
		"app.js": {Data: []byte("console.log('hi')")},
	}
	m := mustMapper(t, assetmapper.Config{Roots: []assetmapper.Root{{FS: fs}}})

	url, err := m.Asset("app.js")
	if err != nil {
		t.Fatal(err)
	}
	// Assert URL shape instead of coupling the test to HashLength.
	if url[:len("/assets/app-")] != "/assets/app-" {
		t.Errorf("url = %q, want /assets/app-<hash>.js", url)
	}
	if url[len(url)-3:] != ".js" {
		t.Errorf("url = %q, want .js extension preserved", url)
	}
}

func TestAsset_StableAcrossCalls(t *testing.T) {
	fs := fstest.MapFS{
		"styles/site.css": {Data: []byte("body{}")},
	}
	m := mustMapper(t, assetmapper.Config{Roots: []assetmapper.Root{{FS: fs}}})

	a, _ := m.Asset("styles/site.css")
	b, _ := m.Asset("styles/site.css")
	if a != b {
		t.Errorf("Asset returned different URLs across calls: %q vs %q", a, b)
	}
}

func TestAsset_DevReadIsLive(t *testing.T) {
	fs := fstest.MapFS{
		"app.js": {Data: []byte("one")},
	}
	m := mustMapper(t, assetmapper.Config{Roots: []assetmapper.Root{{FS: fs}}})

	before, err := m.Asset("app.js")
	if err != nil {
		t.Fatal(err)
	}
	fs["app.js"] = &fstest.MapFile{Data: []byte("two")}
	after, err := m.Asset("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatalf("URL did not change after the source edit: %s", before)
	}

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", after, nil))
	if rec.Body.String() != "two" {
		t.Errorf("served content = %q, want the edited source", rec.Body.String())
	}
}

func TestAsset_FirstRootWins(t *testing.T) {
	user := fstest.MapFS{
		"app.js": {Data: []byte("USER")},
	}
	lib := fstest.MapFS{
		"app.js": {Data: []byte("LIB")},
	}
	m := mustMapper(t, assetmapper.Config{
		Roots: []assetmapper.Root{{FS: user}, {FS: lib}},
	})

	url, err := m.Asset("app.js")
	if err != nil {
		t.Fatal(err)
	}
	// Hash should be of "USER" not "LIB". Verify by re-hashing the
	// shadowed library file and ensuring the URL is NOT that hash.
	libOnly := mustMapper(t, assetmapper.Config{
		Roots: []assetmapper.Root{{FS: lib}},
	})
	libURL, _ := libOnly.Asset("app.js")
	if url == libURL {
		t.Errorf("shadowed root won: url = %q matches library content", url)
	}
}

func TestAsset_MountAtNamespacesRoot(t *testing.T) {
	libAssets := fstest.MapFS{
		"styles.css": {Data: []byte("body{color:red}")},
	}
	m := mustMapper(t, assetmapper.Config{
		Roots: []assetmapper.Root{{FS: libAssets, MountAt: "jobs"}},
	})

	url, err := m.Asset("jobs/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	if url[:len("/assets/jobs/styles-")] != "/assets/jobs/styles-" {
		t.Errorf("url = %q, want /assets/jobs/styles-<hash>.css", url)
	}

	// Without the mount prefix the asset is unreachable.
	if _, err := m.Asset("styles.css"); !errors.Is(err, assetmapper.ErrAssetNotFound) {
		t.Errorf("Asset without mount prefix: err = %v, want ErrAssetNotFound", err)
	}
}

func TestAsset_NotFoundForMissingPath(t *testing.T) {
	m := mustMapper(t, assetmapper.Config{
		Roots: []assetmapper.Root{{FS: fstest.MapFS{}}},
	})
	if _, err := m.Asset("missing.js"); !errors.Is(err, assetmapper.ErrAssetNotFound) {
		t.Errorf("err = %v, want ErrAssetNotFound", err)
	}
}

func TestAsset_RejectsTraversal(t *testing.T) {
	m := mustMapper(t, assetmapper.Config{
		Roots: []assetmapper.Root{{FS: fstest.MapFS{"app.js": {Data: []byte("x")}}}},
	})
	if _, err := m.Asset("../etc/passwd"); !errors.Is(err, assetmapper.ErrAssetNotFound) {
		t.Errorf("err = %v, want ErrAssetNotFound for traversal", err)
	}
}

func TestAsset_ProdModeReadsManifest(t *testing.T) {
	manifest := &assetmapper.Manifest{Entries: map[string]string{
		"app.js": "app-deadbeef.js",
	}}
	m := mustMapper(t, assetmapper.Config{
		Roots:    []assetmapper.Root{{FS: fstest.MapFS{}}}, // empty in prod, manifest is authoritative
		Manifest: manifest,
	})
	url, err := m.Asset("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if url != "/assets/app-deadbeef.js" {
		t.Errorf("url = %q, want /assets/app-deadbeef.js", url)
	}
}

func TestAsset_ProdModeMissingFromManifestIsError(t *testing.T) {
	m := mustMapper(t, assetmapper.Config{
		Roots:    []assetmapper.Root{{FS: fstest.MapFS{"app.js": {Data: []byte("x")}}}},
		Manifest: &assetmapper.Manifest{Entries: map[string]string{}},
	})
	// Even though app.js exists in the source tree, prod mode
	// authoritatively trusts the manifest. A missing entry signals
	// that the caller compiled with a stale view of the assets.
	if _, err := m.Asset("app.js"); !errors.Is(err, assetmapper.ErrAssetNotFound) {
		t.Errorf("err = %v, want ErrAssetNotFound", err)
	}
}

// --- HTTP handler ---

func TestHandler_ServesContent(t *testing.T) {
	fs := fstest.MapFS{"app.js": {Data: []byte("console.log('hi')")}}
	m := mustMapper(t, assetmapper.Config{Roots: []assetmapper.Root{{FS: fs}}})

	url, _ := m.Asset("app.js")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	m.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if string(body) != "console.log('hi')" {
		t.Errorf("body = %q, want %q", body, "console.log('hi')")
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") && !strings.HasPrefix(ct, "application/javascript") {
		t.Errorf("Content-Type = %q, want text/javascript or application/javascript prefix", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache (dev mode should not advertise immutable caching)", cc)
	}
	if rec.Header().Get("ETag") == "" {
		t.Error("ETag missing")
	}
}

func TestHandler_HonoursIfNoneMatch(t *testing.T) {
	fs := fstest.MapFS{"app.js": {Data: []byte("x")}}
	m := mustMapper(t, assetmapper.Config{Roots: []assetmapper.Root{{FS: fs}}})

	url, _ := m.Asset("app.js")
	// Prime to know the etag.
	primer := httptest.NewRecorder()
	m.Handler().ServeHTTP(primer, httptest.NewRequest(http.MethodGet, url, nil))
	etag := primer.Header().Get("ETag")

	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("If-None-Match", etag)
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Errorf("status = %d, want 304", rec.Code)
	}
}

func TestHandler_HEAD(t *testing.T) {
	fs := fstest.MapFS{"app.js": {Data: []byte("body")}}
	m := mustMapper(t, assetmapper.Config{Roots: []assetmapper.Root{{FS: fs}}})

	url, _ := m.Asset("app.js")
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodHead, url, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD body length = %d, want 0", rec.Body.Len())
	}
}

func TestHandler_StaleHashStillServesContent(t *testing.T) {
	// The hash in the URL is purely a cache buster. A request with
	// the wrong hash (e.g. an old bookmark, an in-flight client that
	// hasn't refreshed) must still serve the current content.
	fs := fstest.MapFS{"app.js": {Data: []byte("x")}}
	m := mustMapper(t, assetmapper.Config{Roots: []assetmapper.Root{{FS: fs}}})

	stale := "/assets/app-00000000.js"
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, stale, nil))
	if rec.Code != http.StatusOK {
		t.Errorf("stale hash status = %d, want 200 (hash is cache-buster only)", rec.Code)
	}
}

func TestHandler_404ForUnknownAsset(t *testing.T) {
	m := mustMapper(t, assetmapper.Config{
		Roots: []assetmapper.Root{{FS: fstest.MapFS{}}},
	})
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/missing-abcdef00.js", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandler_404OutsideURLPrefix(t *testing.T) {
	m := mustMapper(t, assetmapper.Config{
		Roots: []assetmapper.Root{{FS: fstest.MapFS{"app.js": {Data: []byte("x")}}}},
	})
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/elsewhere/app-deadbeef.js", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandler_405ForOtherMethods(t *testing.T) {
	m := mustMapper(t, assetmapper.Config{
		Roots: []assetmapper.Root{{FS: fstest.MapFS{"app.js": {Data: []byte("x")}}}},
	})
	url, _ := m.Asset("app.js")
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, url, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

func TestHandler_ProdModeRefusesAllRequests(t *testing.T) {
	// Prod mode is meant to be served by a static file server over
	// the compiled publicDir; routing through the Mapper handler is
	// not supported.
	m := mustMapper(t, assetmapper.Config{
		Roots: []assetmapper.Root{{FS: fstest.MapFS{}}},
		Manifest: &assetmapper.Manifest{Entries: map[string]string{
			"app.js": "app-deadbeef.js",
		}},
	})
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/app-deadbeef.js", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("prod-mode status = %d, want 404", rec.Code)
	}
}

// --- Manifest round-trip ---

func TestManifest_RoundTrip(t *testing.T) {
	want := &assetmapper.Manifest{
		URLPrefix: "/assets/",
		Entries: map[string]string{
			"app.js":          "app-deadbeef.js",
			"images/logo.png": "images/logo-cafef00d.png",
		},
	}
	dir := t.TempDir()
	if err := want.Save(dir); err != nil {
		t.Fatal(err)
	}
	got, err := assetmapper.LoadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.URLPrefix != want.URLPrefix {
		t.Errorf("URLPrefix = %q, want %q", got.URLPrefix, want.URLPrefix)
	}
	if len(got.Entries) != len(want.Entries) {
		t.Fatalf("len(Entries) = %d, want %d", len(got.Entries), len(want.Entries))
	}
	for k, v := range want.Entries {
		if got.Entries[k] != v {
			t.Errorf("Entries[%q] = %q, want %q", k, got.Entries[k], v)
		}
	}
}
