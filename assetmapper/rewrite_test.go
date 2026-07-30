package assetmapper_test

import (
	"os"
	"path/filepath"
	"strconv"
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
	data, err := os.ReadFile(filepath.Join(dir, hashed)) // #nosec G304 -- reads an app-selected asset path
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

func TestCompile_RejectsBareSpecifierAbsentFromImportmap(t *testing.T) {
	for _, specifier := range []string{"react", "#internal", ".hidden", "...pkg"} {
		t.Run(specifier, func(t *testing.T) {
			src := fstest.MapFS{
				"app.js": {Data: []byte(`import ` + strconv.Quote(specifier) + `;`)},
			}
			_, err := assetmapper.Compile([]assetmapper.Root{{FS: src}}, t.TempDir())
			if err == nil || !strings.Contains(err.Error(), "absent from the importmap") {
				t.Fatalf("Compile error = %v, want bare-import rejection", err)
			}
		})
	}
}

func TestCompile_IgnoresJSImportTextOutsideModuleSyntax(t *testing.T) {
	src := fstest.MapFS{
		"app.js": {Data: []byte(`
export const label = "./not-an-import.js";
export default "also-not-an-import.js";
const text = 'import x from "./string.js"';
const pattern = /import x from "\.\/regex.js"/;
// import x from "./line-comment.js";
/* export * from "./block-comment.js"; */
const object = {import(specifier) { return specifier }};
object.import("./method.js");
class Example {
  #import(specifier) { return specifier }
  load() { return this.#import("./private-method.js") }
}
πimport("./unicode-identifier.js");
export default ` + "`from \"./template-text.js\"`" + `;
export default /from "\.\/regex-text.js"/;
if (true) /import "regex-body"/.test(text);
async function scan(stream) {
  for await (const item of stream) /import "async-regex-body"/.test(item);
}
class Pattern extends /import "extends-regex-body"/ {}
debugger
/import "debugger-regex-body"/.test(text);
`)},
	}
	if _, err := assetmapper.Compile([]assetmapper.Root{{FS: src}}, t.TempDir()); err != nil {
		t.Fatalf("non-module text became a dependency: %v", err)
	}
}

func TestCompile_IgnoresJavaScriptHashbangComment(t *testing.T) {
	for _, prefix := range []string{"", "\ufeff"} {
		src := fstest.MapFS{
			"app.js": {Data: []byte(prefix + "#!/usr/bin/env node import \"./not-a-module.js\"\nexport {};")},
		}
		if _, err := assetmapper.Compile([]assetmapper.Root{{FS: src}}, t.TempDir()); err != nil {
			t.Fatalf("hashbang text became a dependency: %v", err)
		}
	}
}

func TestCompile_FindsDynamicImportInTemplateExpression(t *testing.T) {
	src := fstest.MapFS{
		"app.js": {Data: []byte("const value = `result: ${import(\"./missing.js\")}`;")},
	}
	_, err := assetmapper.Compile([]assetmapper.Root{{FS: src}}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "missing asset") {
		t.Fatalf("Compile error = %v, want dynamic-import rejection", err)
	}
}

func TestCompile_RewritesNoSubstitutionTemplateImport(t *testing.T) {
	missing := fstest.MapFS{
		"app.js": {Data: []byte("const value = import(`./missing.js`);")},
	}
	if _, err := assetmapper.Compile([]assetmapper.Root{{FS: missing}}, t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "missing asset") {
		t.Fatalf("Compile error = %v, want template-import rejection", err)
	}

	src := fstest.MapFS{
		"app.js": {Data: []byte("const value = import(`./dependency.js`);")},
		"dependency.js": {
			Data: []byte("export default {};"),
		},
	}
	out, manifest := compileAndRead(t, src, "app.js")
	if want := "/assets/" + manifest.Entries["dependency.js"]; !strings.Contains(out, "`"+want+"`") {
		t.Fatalf("template import was not rewritten to %q:\n%s", want, out)
	}
}

func TestCompile_DecodesEscapedAssetSpecifiers(t *testing.T) {
	t.Run("JavaScript", func(t *testing.T) {
		src := fstest.MapFS{
			"app.js": {Data: []byte(`import "./fo\u006f.js";`)},
			"foo.js": {Data: []byte(`export {}`)},
		}
		out, manifest := compileAndRead(t, src, "app.js")
		if want := "/assets/" + manifest.Entries["foo.js"]; !strings.Contains(out, want) {
			t.Fatalf("escaped JavaScript specifier was not rewritten to %q:\n%s", want, out)
		}
	})
	t.Run("CSS", func(t *testing.T) {
		src := fstest.MapFS{
			"app.css": {Data: []byte(`
.quoted{background:url("im\61ge.png")}
.unquoted{background:url(im\61 ge.png)}
`)},
			"image.png": {Data: []byte("image")},
		}
		out, manifest := compileAndRead(t, src, "app.css")
		if want := "/assets/" + manifest.Entries["image.png"]; strings.Count(out, want) != 2 {
			t.Fatalf("escaped CSS specifier was not rewritten to %q:\n%s", want, out)
		}
	})
	t.Run("CSS Unicode simple escape", func(t *testing.T) {
		src := fstest.MapFS{
			"app.css": {Data: []byte(`
.quoted{background:url("\π.png")}
.unquoted{background:url(\π.png)}
`)},
			"π.png": {Data: []byte("image")},
		}
		out, manifest := compileAndRead(t, src, "app.css")
		if want := "/assets/" + manifest.Entries["π.png"]; strings.Count(out, want) != 2 {
			t.Fatalf("Unicode CSS escape was not rewritten to %q:\n%s", want, out)
		}
	})
}

func TestCompile_AllowsBareSpecifierFromImportmap(t *testing.T) {
	src := fstest.MapFS{
		"app.js":          {Data: []byte(`import React from "react";`)},
		"importmap.json":  {Data: []byte(`{"react":{"version":"1.0.0"}}`)},
		"vendor/react.js": {Data: []byte(`export default {}`)},
	}
	out, _ := compileAndRead(t, src, "app.js")
	if !strings.Contains(out, `"react"`) {
		t.Fatalf("bare import was changed: %s", out)
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

func TestCompile_RejectsMissingRelativeJSImport(t *testing.T) {
	src := fstest.MapFS{
		"app.js": {Data: []byte(`import x from "./missing.js";`)},
	}
	_, err := assetmapper.Compile([]assetmapper.Root{{FS: src}}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), `references missing asset "./missing.js"`) {
		t.Fatalf("Compile error = %v, want missing-import rejection", err)
	}
}

func TestCompile_HandlesJavaScriptUnicodeWhitespace(t *testing.T) {
	t.Run("NBSP separates static import", func(t *testing.T) {
		src := fstest.MapFS{
			"app.js": {Data: []byte("import\u00a0\"./missing.js\";")},
		}
		_, err := assetmapper.Compile([]assetmapper.Root{{FS: src}}, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "missing asset") {
			t.Fatalf("Compile error = %v, want NBSP-separated import rejection", err)
		}
	})
	t.Run("Unicode line separator ends line comment", func(t *testing.T) {
		src := fstest.MapFS{
			"app.js": {Data: []byte("// comment\u2028import \"./missing.js\";")},
		}
		_, err := assetmapper.Compile([]assetmapper.Root{{FS: src}}, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "missing asset") {
			t.Fatalf("Compile error = %v, want post-comment import rejection", err)
		}
	})
	t.Run("Unicode ASI separator preserves regex context", func(t *testing.T) {
		src := fstest.MapFS{
			"app.js": {Data: []byte("debugger\u2028/import \"not-a-module\"/.test(text);")},
		}
		if _, err := assetmapper.Compile([]assetmapper.Root{{FS: src}}, t.TempDir()); err != nil {
			t.Fatalf("regex body became an import: %v", err)
		}
	})
}

func TestCompile_ConfiguresExternalStyleAndRootURLs(t *testing.T) {
	t.Run("root relative default external", func(t *testing.T) {
		src := fstest.MapFS{"app.js": {Data: []byte(`import "/runtime/module.js";`)}}
		if _, err := assetmapper.Compile([]assetmapper.Root{{FS: src}}, t.TempDir()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("root relative strict", func(t *testing.T) {
		src := fstest.MapFS{"app.js": {Data: []byte(`import "/runtime/module.js";`)}}
		_, err := assetmapper.Compile(
			[]assetmapper.Root{{FS: src}},
			t.TempDir(),
			assetmapper.WithStrictAssetURLs(),
		)
		if err == nil || !strings.Contains(err.Error(), "missing asset") {
			t.Fatalf("Compile error = %v, want strict root-relative rejection", err)
		}
	})
	t.Run("invalid root relative strict", func(t *testing.T) {
		src := fstest.MapFS{"app.js": {Data: []byte(`import "/runtime/../module.js";`)}}
		_, err := assetmapper.Compile(
			[]assetmapper.Root{{FS: src}},
			t.TempDir(),
			assetmapper.WithStrictAssetURLs(),
		)
		if err == nil || !strings.Contains(err.Error(), "invalid root-relative asset") {
			t.Fatalf("Compile error = %v, want invalid root-relative rejection", err)
		}
	})
	t.Run("CSS URL default external", func(t *testing.T) {
		src := fstest.MapFS{"app.css": {Data: []byte(`body{background:url("./runtime.png")}`)}}
		if _, err := assetmapper.Compile([]assetmapper.Root{{FS: src}}, t.TempDir()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("CSS URL strict", func(t *testing.T) {
		src := fstest.MapFS{"app.css": {Data: []byte(`body{background:url("./runtime.png")}`)}}
		_, err := assetmapper.Compile(
			[]assetmapper.Root{{FS: src}},
			t.TempDir(),
			assetmapper.WithStrictAssetURLs(),
		)
		if err == nil || !strings.Contains(err.Error(), "missing asset") {
			t.Fatalf("Compile error = %v, want strict CSS URL rejection", err)
		}
	})
	t.Run("invalid root CSS URL strict", func(t *testing.T) {
		src := fstest.MapFS{"app.css": {Data: []byte(`body{background:url("/../runtime.png")}`)}}
		_, err := assetmapper.Compile(
			[]assetmapper.Root{{FS: src}},
			t.TempDir(),
			assetmapper.WithStrictAssetURLs(),
		)
		if err == nil || !strings.Contains(err.Error(), "invalid root-relative CSS URL") {
			t.Fatalf("Compile error = %v, want invalid root-relative CSS URL rejection", err)
		}
	})
	t.Run("CSS import always strict", func(t *testing.T) {
		src := fstest.MapFS{"app.css": {Data: []byte(`@import url("./missing.css");`)}}
		_, err := assetmapper.Compile([]assetmapper.Root{{FS: src}}, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "missing asset") {
			t.Fatalf("Compile error = %v, want CSS import rejection", err)
		}
	})
	t.Run("commented CSS references ignored", func(t *testing.T) {
		src := fstest.MapFS{"app.css": {Data: []byte(`
/* @import "./missing.css"; */
/* body { background: url("./missing.png") } */
`)}}
		if _, err := assetmapper.Compile(
			[]assetmapper.Root{{FS: src}},
			t.TempDir(),
			assetmapper.WithStrictAssetURLs(),
		); err != nil {
			t.Fatalf("commented CSS became a dependency: %v", err)
		}
	})
	t.Run("comment then uppercase CSS import is strict", func(t *testing.T) {
		src := fstest.MapFS{"app.css": {Data: []byte(`/* license */ @IMPORT URL("./missing.css");`)}}
		_, err := assetmapper.Compile([]assetmapper.Root{{FS: src}}, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "missing asset") {
			t.Fatalf("Compile error = %v, want uppercase CSS import rejection", err)
		}
	})
	t.Run("escaped CSS import keyword is strict", func(t *testing.T) {
		src := fstest.MapFS{"app.css": {Data: []byte(`@\69mport "./missing.css";`)}}
		_, err := assetmapper.Compile([]assetmapper.Root{{FS: src}}, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "missing asset") {
			t.Fatalf("Compile error = %v, want escaped CSS import rejection", err)
		}
	})
	t.Run("escaped CSS URL function honors strict mode", func(t *testing.T) {
		src := fstest.MapFS{"app.css": {Data: []byte(`body{background:u\72l("./missing.png")}`)}}
		_, err := assetmapper.Compile(
			[]assetmapper.Root{{FS: src}},
			t.TempDir(),
			assetmapper.WithStrictAssetURLs(),
		)
		if err == nil || !strings.Contains(err.Error(), "missing asset") {
			t.Fatalf("Compile error = %v, want escaped CSS URL rejection", err)
		}
	})
	t.Run("similar at-rule remains configurable CSS URL", func(t *testing.T) {
		src := fstest.MapFS{"app.css": {Data: []byte(`@important url("./runtime.png");`)}}
		if _, err := assetmapper.Compile([]assetmapper.Root{{FS: src}}, t.TempDir()); err != nil {
			t.Fatalf("ordinary CSS URL was treated as a strict import: %v", err)
		}
	})
	t.Run("bare CSS URL resolves relative", func(t *testing.T) {
		src := fstest.MapFS{
			"styles/app.css":   {Data: []byte(`body{background:url("image.png")}`)},
			"styles/image.png": {Data: []byte("image")},
		}
		out, manifest := compileAndRead(t, src, "styles/app.css")
		if !strings.Contains(out, "/assets/"+manifest.Entries["styles/image.png"]) {
			t.Fatalf("bare CSS URL was not rewritten: %s", out)
		}
	})
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

// --- Circular dependency graphs ---

func TestCompile_SupportsImportCycle(t *testing.T) {
	src := fstest.MapFS{
		"a.js": {Data: []byte(`import b from "./b.js"; export default b;`)},
		"b.js": {Data: []byte(`import a from "./a.js"; export default a;`)},
	}
	dir := t.TempDir()
	manifest, err := assetmapper.Compile([]assetmapper.Root{{FS: src}}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if outputDigest(manifest.Entries["a.js"]) != outputDigest(manifest.Entries["b.js"]) {
		t.Fatalf("cycle members do not share a digest: %v", manifest.Entries)
	}
	for logical, dependency := range map[string]string{"a.js": "b.js", "b.js": "a.js"} {
		if got := manifest.Dependencies[logical]; len(got) != 1 || got[0] != dependency {
			t.Errorf("persisted %s dependencies = %v, want [%s]", logical, got, dependency)
		}
		content, err := os.ReadFile(filepath.Join(dir, manifest.Entries[logical]))
		if err != nil {
			t.Fatal(err)
		}
		if want := "/assets/" + manifest.Entries[dependency]; !strings.Contains(string(content), want) {
			t.Fatalf("%s does not reference cyclic dependency %q:\n%s", logical, want, content)
		}
	}
	repeated, err := assetmapper.Compile([]assetmapper.Root{{FS: src}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, logical := range []string{"a.js", "b.js"} {
		if manifest.Entries[logical] != repeated.Entries[logical] {
			t.Errorf("%s cycle output is nondeterministic: %q != %q",
				logical, manifest.Entries[logical], repeated.Entries[logical])
		}
	}
}

func TestCompile_CycleChangeInvalidatesComponentAndImporters(t *testing.T) {
	version := func(bValue string) fstest.MapFS {
		return fstest.MapFS{
			"entry.js": {Data: []byte(`import "./a.js";`)},
			"a.js":     {Data: []byte(`import "./b.js"; export const a = 1;`)},
			"b.js":     {Data: []byte(`import "./a.js"; export const b = ` + bValue + `;`)},
		}
	}
	first, err := assetmapper.Compile([]assetmapper.Root{{FS: version("1")}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	second, err := assetmapper.Compile([]assetmapper.Root{{FS: version("2")}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, logical := range []string{"a.js", "b.js", "entry.js"} {
		if first.Entries[logical] == second.Entries[logical] {
			t.Errorf("%s was not invalidated by a cyclic dependency change", logical)
		}
	}
}

func TestCompile_ExternalDependencyInvalidatesCycle(t *testing.T) {
	version := func(value string) fstest.MapFS {
		return fstest.MapFS{
			"a.js":    {Data: []byte(`import "./b.js"; import "./util.js";`)},
			"b.js":    {Data: []byte(`import "./a.js";`)},
			"util.js": {Data: []byte(`export const value = ` + value + `;`)},
		}
	}
	first, err := assetmapper.Compile([]assetmapper.Root{{FS: version("1")}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	second, err := assetmapper.Compile([]assetmapper.Root{{FS: version("2")}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, logical := range []string{"a.js", "b.js", "util.js"} {
		if first.Entries[logical] == second.Entries[logical] {
			t.Errorf("%s was not invalidated by the external dependency change", logical)
		}
	}
}

func TestCompile_CycleDigestIncludesURLPrefix(t *testing.T) {
	src := fstest.MapFS{
		"a.js": {Data: []byte(`import "./b.js";`)},
		"b.js": {Data: []byte(`import "./a.js";`)},
	}
	defaultManifest, err := assetmapper.Compile([]assetmapper.Root{{FS: src}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	customDir := t.TempDir()
	customManifest, err := assetmapper.Compile(
		[]assetmapper.Root{{FS: src}},
		customDir,
		assetmapper.WithURLPrefix("/static/"),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, logical := range []string{"a.js", "b.js"} {
		if defaultManifest.Entries[logical] == customManifest.Entries[logical] {
			t.Errorf("%s digest did not include the rewritten URL prefix", logical)
		}
	}
	content, err := os.ReadFile(filepath.Join(customDir, customManifest.Entries["a.js"]))
	if err != nil {
		t.Fatal(err)
	}
	if want := "/static/" + customManifest.Entries["b.js"]; !strings.Contains(string(content), want) {
		t.Fatalf("custom prefix missing from cyclic rewrite %q:\n%s", want, content)
	}
}

func TestCompile_SupportsSelfImport(t *testing.T) {
	src := fstest.MapFS{
		"self.js": {Data: []byte(`import "./self.js";`)},
	}
	out, manifest := compileAndRead(t, src, "self.js")
	if want := "/assets/" + manifest.Entries["self.js"]; !strings.Contains(out, want) {
		t.Fatalf("self import was not rewritten to %q:\n%s", want, out)
	}
}

func TestCompile_SupportsStylesheetCycle(t *testing.T) {
	src := fstest.MapFS{
		"a.css": {Data: []byte(`@import "./b.css";`)},
		"b.css": {Data: []byte(`@import "./a.css";`)},
	}
	dir := t.TempDir()
	manifest, err := assetmapper.Compile([]assetmapper.Root{{FS: src}}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if outputDigest(manifest.Entries["a.css"]) != outputDigest(manifest.Entries["b.css"]) {
		t.Fatalf("stylesheet cycle members do not share a digest: %v", manifest.Entries)
	}
	for logical, dependency := range map[string]string{"a.css": "b.css", "b.css": "a.css"} {
		content, err := os.ReadFile(filepath.Join(dir, manifest.Entries[logical]))
		if err != nil {
			t.Fatal(err)
		}
		if want := "/assets/" + manifest.Entries[dependency]; !strings.Contains(string(content), want) {
			t.Fatalf("%s does not reference cyclic stylesheet %q:\n%s", logical, want, content)
		}
	}
}

func outputDigest(output string) string {
	extension := filepath.Ext(output)
	stem := strings.TrimSuffix(output, extension)
	return stem[strings.LastIndexByte(stem, '-')+1:]
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
	data, _ := os.ReadFile(filepath.Join(dir, manifest.Entries["app.js"])) // #nosec G304 -- reads an app-selected asset path
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
	data, _ := os.ReadFile(filepath.Join(dir, manifest.Entries["app.js"])) // #nosec G304 -- reads an app-selected asset path
	want := "/static/" + manifest.Entries["util.js"]
	if !strings.Contains(string(data), want) {
		t.Errorf("rewritten url missing trailing-slash normalisation; got:\n%s", data)
	}
}
