package assetmapper_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gofabrik/fabrik/assetmapper"
)

// helper: compile and return the rewritten content of a logical path.
func compileAndRead(t *testing.T, src fstest.MapFS, logical string) (string, *assetmapper.Manifest) {
	t.Helper()
	dir := t.TempDir()
	manifest, err := assetmapper.Compile([]assetmapper.Root{{FS: src}}, dir)
	if err != nil {
		t.Fatal(err)
	}
	hashed, ok := manifest.Entries[logical]
	if !ok {
		t.Fatalf("manifest missing %q; entries = %v", logical, manifest.Entries)
	}
	data, err := os.ReadFile(filepath.Join(dir, hashed))
	if err != nil {
		t.Fatal(err)
	}
	return string(data), manifest
}

// --- JS rewriting ---

func TestCompile_RewritesJSStaticImport(t *testing.T) {
	src := fstest.MapFS{
		"app.js":  {Data: []byte(`import util from "./util.js";` + "\nconsole.log(util)")},
		"util.js": {Data: []byte(`export default 1`)},
	}
	out, manifest := compileAndRead(t, src, "app.js")
	wantURL := "/assets/" + manifest.Entries["util.js"]
	if !strings.Contains(out, `"`+wantURL+`"`) {
		t.Errorf("rewritten app.js missing %q; got:\n%s", wantURL, out)
	}
	if strings.Contains(out, `"./util.js"`) {
		t.Errorf("original specifier still present; got:\n%s", out)
	}
}

func TestCompile_RewritesJSExportFrom(t *testing.T) {
	src := fstest.MapFS{
		"index.js": {Data: []byte(`export { foo } from "./util.js"; export * from "./other.js";`)},
		"util.js":  {Data: []byte(`export const foo = 1`)},
		"other.js": {Data: []byte(`export const bar = 2`)},
	}
	out, manifest := compileAndRead(t, src, "index.js")
	wantUtil := "/assets/" + manifest.Entries["util.js"]
	wantOther := "/assets/" + manifest.Entries["other.js"]
	if !strings.Contains(out, wantUtil) {
		t.Errorf("missing rewritten util URL %q; got:\n%s", wantUtil, out)
	}
	if !strings.Contains(out, wantOther) {
		t.Errorf("missing rewritten other URL %q; got:\n%s", wantOther, out)
	}
}

func TestCompile_RewritesJSDynamicImport(t *testing.T) {
	src := fstest.MapFS{
		"app.js":  {Data: []byte(`const m = await import("./lazy.js");`)},
		"lazy.js": {Data: []byte(`export default {}`)},
	}
	out, manifest := compileAndRead(t, src, "app.js")
	want := "/assets/" + manifest.Entries["lazy.js"]
	if !strings.Contains(out, want) {
		t.Errorf("missing rewritten dynamic import %q; got:\n%s", want, out)
	}
}

func TestCompile_RewritesJSMultiLineImport(t *testing.T) {
	src := fstest.MapFS{
		"app.js": {Data: []byte(`import {
  a,
  b,
  c,
} from "./util.js";`)},
		"util.js": {Data: []byte(`export const a=1, b=2, c=3`)},
	}
	out, manifest := compileAndRead(t, src, "app.js")
	want := "/assets/" + manifest.Entries["util.js"]
	if !strings.Contains(out, want) {
		t.Errorf("missing rewritten multi-line import %q; got:\n%s", want, out)
	}
}

func TestCompile_LeavesBareSpecifierAlone(t *testing.T) {
	src := fstest.MapFS{
		"app.js": {Data: []byte(`import React from "react"; console.log(React);`)},
	}
	out, _ := compileAndRead(t, src, "app.js")
	if !strings.Contains(out, `"react"`) {
		t.Errorf("bare specifier was rewritten; got:\n%s", out)
	}
}

func TestCompile_LeavesExternalURLAlone(t *testing.T) {
	src := fstest.MapFS{
		"app.js": {Data: []byte(`import x from "https://cdn.example.com/x.js";`)},
	}
	out, _ := compileAndRead(t, src, "app.js")
	if !strings.Contains(out, `"https://cdn.example.com/x.js"`) {
		t.Errorf("external URL was rewritten; got:\n%s", out)
	}
}

func TestCompile_LeavesUnresolvableRefAlone(t *testing.T) {
	// Missing local references stay unchanged.
	src := fstest.MapFS{
		"app.js": {Data: []byte(`import x from "./missing.js";`)},
	}
	out, _ := compileAndRead(t, src, "app.js")
	if !strings.Contains(out, `"./missing.js"`) {
		t.Errorf("missing-target ref was rewritten; got:\n%s", out)
	}
}

func TestCompile_AbsolutePathRefRewrittenToAbsoluteURL(t *testing.T) {
	src := fstest.MapFS{
		"sub/app.js": {Data: []byte(`import g from "/global.js";`)},
		"global.js":  {Data: []byte(`export default {}`)},
	}
	out, manifest := compileAndRead(t, src, "sub/app.js")
	want := "/assets/" + manifest.Entries["global.js"]
	if !strings.Contains(out, want) {
		t.Errorf("absolute ref not rewritten; got:\n%s", out)
	}
}

// --- CSS rewriting ---

func TestCompile_RewritesCSSURLAllQuoteForms(t *testing.T) {
	src := fstest.MapFS{
		"styles.css": {Data: []byte(`a{background:url("./img/a.png")} b{background:url('./img/b.png')} c{background:url(./img/c.png)}`)},
		"img/a.png":  {Data: []byte("A")},
		"img/b.png":  {Data: []byte("B")},
		"img/c.png":  {Data: []byte("C")},
	}
	out, manifest := compileAndRead(t, src, "styles.css")
	for _, k := range []string{"img/a.png", "img/b.png", "img/c.png"} {
		want := "/assets/" + manifest.Entries[k]
		if !strings.Contains(out, want) {
			t.Errorf("%s URL %q not present; got:\n%s", k, want, out)
		}
	}
}

func TestCompile_RewritesCSSAtImportString(t *testing.T) {
	src := fstest.MapFS{
		"main.css":  {Data: []byte(`@import "./reset.css"; body{}`)},
		"reset.css": {Data: []byte(`*{margin:0}`)},
	}
	out, manifest := compileAndRead(t, src, "main.css")
	want := "/assets/" + manifest.Entries["reset.css"]
	if !strings.Contains(out, want) {
		t.Errorf("@import not rewritten; got:\n%s", out)
	}
}

func TestCompile_RewritesCSSAtImportURL(t *testing.T) {
	src := fstest.MapFS{
		"main.css":  {Data: []byte(`@import url("./reset.css"); body{}`)},
		"reset.css": {Data: []byte(`*{margin:0}`)},
	}
	out, manifest := compileAndRead(t, src, "main.css")
	want := "/assets/" + manifest.Entries["reset.css"]
	if !strings.Contains(out, want) {
		t.Errorf("@import url() not rewritten; got:\n%s", out)
	}
}

func TestCompile_CSSLeavesDataURIAlone(t *testing.T) {
	src := fstest.MapFS{
		"styles.css": {Data: []byte(`a{background:url(data:image/png;base64,iVBORw0)}`)},
	}
	out, _ := compileAndRead(t, src, "styles.css")
	if !strings.Contains(out, `data:image/png;base64,iVBORw0`) {
		t.Errorf("data URI was rewritten; got:\n%s", out)
	}
}

func TestCompile_CSSLeavesSVGFragmentAlone(t *testing.T) {
	src := fstest.MapFS{
		"styles.css": {Data: []byte(`a{fill:url(#myGradient)}`)},
	}
	out, _ := compileAndRead(t, src, "styles.css")
	if !strings.Contains(out, `url(#myGradient)`) {
		t.Errorf("SVG fragment was rewritten; got:\n%s", out)
	}
}

func TestCompile_CSSRelativeWithParentTraversal(t *testing.T) {
	src := fstest.MapFS{
		"styles/main.css": {Data: []byte(`a{background:url("../images/logo.png")}`)},
		"images/logo.png": {Data: []byte("PNG")},
	}
	out, manifest := compileAndRead(t, src, "styles/main.css")
	want := "/assets/" + manifest.Entries["images/logo.png"]
	if !strings.Contains(out, want) {
		t.Errorf("../images/logo.png not resolved; got:\n%s", out)
	}
}

// --- Transitive hashing ---

func TestCompile_TransitiveHashChangeOnDepUpdate(t *testing.T) {
	// Dependency hash changes propagate to importers.
	v1 := fstest.MapFS{
		"app.js":  {Data: []byte(`import u from "./util.js"; u();`)},
		"util.js": {Data: []byte(`export default function(){}`)},
	}
	v2 := fstest.MapFS{
		"app.js":  {Data: []byte(`import u from "./util.js"; u();`)},
		"util.js": {Data: []byte(`export default function(){ return 1 }`)},
	}
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	m1, err := assetmapper.Compile([]assetmapper.Root{{FS: v1}}, dir1)
	if err != nil {
		t.Fatal(err)
	}
	m2, err := assetmapper.Compile([]assetmapper.Root{{FS: v2}}, dir2)
	if err != nil {
		t.Fatal(err)
	}
	if m1.Entries["util.js"] == m2.Entries["util.js"] {
		t.Fatal("util.js hash did not change despite content change")
	}
	if m1.Entries["app.js"] == m2.Entries["app.js"] {
		t.Errorf("app.js hash did NOT change despite util.js change — transitive cache-busting broken")
	}
}

// --- Cycle detection ---

func TestCompile_DetectsImportCycle(t *testing.T) {
	src := fstest.MapFS{
		"a.js": {Data: []byte(`import b from "./b.js"; export default b;`)},
		"b.js": {Data: []byte(`import a from "./a.js"; export default a;`)},
	}
	_, err := assetmapper.Compile([]assetmapper.Root{{FS: src}}, t.TempDir())
	var cycle *assetmapper.CycleError
	if !errors.As(err, &cycle) {
		t.Fatalf("err = %v, want *CycleError", err)
	}
	if len(cycle.Nodes) != 2 {
		t.Errorf("CycleError.Nodes = %v, want both a.js and b.js", cycle.Nodes)
	}
}

// --- URL prefix options ---

func TestCompile_CustomURLPrefix(t *testing.T) {
	src := fstest.MapFS{
		"app.js":  {Data: []byte(`import u from "./util.js";`)},
		"util.js": {Data: []byte(`export default {}`)},
	}
	dir := t.TempDir()
	manifest, err := assetmapper.Compile(
		[]assetmapper.Root{{FS: src}}, dir,
		assetmapper.WithURLPrefix("/static/v2/"),
	)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, manifest.Entries["app.js"]))
	want := "/static/v2/" + manifest.Entries["util.js"]
	if !strings.Contains(string(data), want) {
		t.Errorf("rewritten url uses default prefix instead of custom; got:\n%s", data)
	}
}

func TestCompile_CustomURLPrefixGetsTrailingSlash(t *testing.T) {
	// No trailing slash on the user's input; Compile must add it
	// so concatenation produces a well-formed URL.
	src := fstest.MapFS{
		"app.js":  {Data: []byte(`import u from "./util.js";`)},
		"util.js": {Data: []byte(`export default {}`)},
	}
	dir := t.TempDir()
	manifest, err := assetmapper.Compile(
		[]assetmapper.Root{{FS: src}}, dir,
		assetmapper.WithURLPrefix("/static"),
	)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, manifest.Entries["app.js"]))
	want := "/static/" + manifest.Entries["util.js"]
	if !strings.Contains(string(data), want) {
		t.Errorf("rewritten url missing trailing-slash normalisation; got:\n%s", data)
	}
}
