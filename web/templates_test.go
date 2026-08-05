package web_test

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gofabrik/fabrik/web"
)

// The package has no layout rule, so these fixtures arrange one the way a
// caller would: a partial that wraps whatever the page calls "content".
const baseLayout = `{{ define "layout" }}<html><body>{{ template "content" . }}</body></html>{{ end }}`

func file(body string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(body)} }

func render(t *testing.T, set *web.Templates, page, define string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := set.Render(&buf, page, define, nil); err != nil {
		t.Fatalf("Render(%q, %q): %v", page, define, err)
	}
	return buf.String()
}

func TestLoad_DefaultSection_BareKeys(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": file(baseLayout),
		"tpl/_default/home.html":    file(`{{ define "content" }}<p>home</p>{{ end }}`),
		"tpl/_default/about.html":   file(`{{ define "content" }}<p>about</p>{{ end }}`),
	}
	set, err := web.LoadTemplates(fsys, "tpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out := render(t, set, "home", "layout"); !strings.Contains(out, "<p>home</p>") {
		t.Errorf("home render missing content: %q", out)
	}
	if out := render(t, set, "about", "layout"); !strings.Contains(out, "<p>about</p>") {
		t.Errorf("about render missing content: %q", out)
	}
	var buf bytes.Buffer
	if err := set.Render(&buf, "_default/home", "layout", nil); err == nil {
		t.Error("default-section pages should be reachable by bare name only")
	}
}

// A fragment is any template the page can see. Rendering one is the same call
// as rendering the whole page with a different name.
func TestRender_FragmentWithoutTheSurroundingPage(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": file(baseLayout),
		"tpl/todo/list.html": file(`
			{{ define "content" }}<ul>{{ template "row" . }}</ul>{{ end }}
			{{ define "row" }}<li>a todo</li>{{ end }}`),
	}
	set, err := web.LoadTemplates(fsys, "tpl", nil)
	if err != nil {
		t.Fatal(err)
	}

	whole := render(t, set, "todo/list", "layout")
	if !strings.Contains(whole, "<html>") || !strings.Contains(whole, "<li>a todo</li>") {
		t.Errorf("whole page = %q", whole)
	}

	fragment := render(t, set, "todo/list", "row")
	if fragment != "<li>a todo</li>" {
		t.Errorf("fragment = %q, want just the row", fragment)
	}
	if strings.Contains(fragment, "<html>") {
		t.Error("fragment must not carry the surrounding document")
	}
}

func TestRender_PartialByItsFileName(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": file(baseLayout),
		"tpl/_default/_flash.html":  file(`<div class="flash">note</div>`),
		"tpl/_default/home.html":    file(`{{ define "content" }}home{{ end }}`),
	}
	set, err := web.LoadTemplates(fsys, "tpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out := render(t, set, "home", "_flash.html"); !strings.Contains(out, "flash") {
		t.Errorf("partial render = %q", out)
	}
}

func TestRender_UnknownDefineIsReported(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": file(baseLayout),
		"tpl/_default/home.html":    file(`{{ define "content" }}home{{ end }}`),
	}
	set, err := web.LoadTemplates(fsys, "tpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	err = set.Render(&buf, "home", "nope", nil)
	if err == nil || !strings.Contains(err.Error(), "defines no template") {
		t.Fatalf("err = %v, want a missing-define error", err)
	}
	if buf.Len() != 0 {
		t.Errorf("nothing should be written, got %q", buf.String())
	}
}

func TestSet_GroupTemplatesVisible(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": file(baseLayout),
		"tpl/todo/list.html": file(`
			{{ define "content" }}c{{ end }}
			{{ define "row" }}r{{ end }}`),
	}
	set, err := web.LoadTemplates(fsys, "tpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Everything the group can see renders by name: the page's own defines
	// and the ones a partial contributes.
	for define, want := range map[string]string{"content": "c", "row": "r"} {
		if got := render(t, set, "todo/list", define); got != want {
			t.Errorf("render %q = %q, want %q", define, got, want)
		}
	}
	var buf bytes.Buffer
	if err := set.Render(&buf, "nope", "content", nil); err == nil {
		t.Error("unknown page should fail to render")
	}
}

func TestLoad_SectionPartialShadowsDefault(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": file(`{{ define "layout" }}DEFAULT {{ template "content" . }}{{ end }}`),
		"tpl/_default/home.html":    file(`{{ define "content" }}home{{ end }}`),
		"tpl/public/_layout.html":   file(`{{ define "layout" }}PUBLIC {{ template "content" . }}{{ end }}`),
		"tpl/public/status.html":    file(`{{ define "content" }}status{{ end }}`),
	}
	set, err := web.LoadTemplates(fsys, "tpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out := render(t, set, "home", "layout"); !strings.Contains(out, "DEFAULT") {
		t.Errorf("home should use the default partial; got %q", out)
	}
	if out := render(t, set, "public/status", "layout"); !strings.Contains(out, "PUBLIC") {
		t.Errorf("public/status should use its own partial; got %q", out)
	}
}

func TestLoad_PartialsAreSharedFromDefault(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": file(baseLayout),
		"tpl/_default/_note.html":   file(`{{ define "note" }}shared note{{ end }}`),
		"tpl/public/status.html":    file(`{{ define "content" }}{{ template "note" . }}{{ end }}`),
	}
	set, err := web.LoadTemplates(fsys, "tpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out := render(t, set, "public/status", "layout"); !strings.Contains(out, "shared note") {
		t.Errorf("render = %q", out)
	}
}

func TestLoad_SectionDefineBeatsDefaultAcrossFilenames(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": file(baseLayout),
		"tpl/_default/_note.html":   file(`{{ define "note" }}default note{{ end }}`),
		"tpl/public/_override.html": file(`{{ define "note" }}section note{{ end }}`),
		"tpl/public/status.html":    file(`{{ define "content" }}{{ template "note" . }}{{ end }}`),
	}
	set, err := web.LoadTemplates(fsys, "tpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out := render(t, set, "public/status", "layout"); !strings.Contains(out, "section note") {
		t.Errorf("section define should win; got %q", out)
	}
}

// A section with no layout in reach is valid: the caller may only ever
// want fragments out of it.
func TestLoad_SectionWithoutAnyLayoutIsFine(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/fragments/row.html": file(`{{ define "row" }}<li>row</li>{{ end }}`),
	}
	set, err := web.LoadTemplates(fsys, "tpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out := render(t, set, "fragments/row", "row"); out != "<li>row</li>" {
		t.Errorf("render = %q", out)
	}
}

// A page need not define "content", or anything at all: its own body is
// addressable by file name.
func TestLoad_PageBodyIsAddressableByFileName(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/mail/welcome.html": file(`Hello {{ . }}`),
	}
	set, err := web.LoadTemplates(fsys, "tpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := set.Render(&buf, "mail/welcome", "welcome.html", "Ada"); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "Hello Ada" {
		t.Errorf("render = %q", buf.String())
	}
}

func TestLoad_DefaultFuncsAvailable(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": file(baseLayout),
		"tpl/_default/home.html":    file(`{{ define "content" }}{{ add 2 3 }}/{{ sub 9 4 }}{{ end }}`),
	}
	set, err := web.LoadTemplates(fsys, "tpl")
	if err != nil {
		t.Fatal(err)
	}
	if out := render(t, set, "home", "layout"); !strings.Contains(out, "5/5") {
		t.Errorf("render = %q", out)
	}
}

func TestLoad_UserFuncsShadowDefaults(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": file(baseLayout),
		"tpl/_default/home.html":    file(`{{ define "content" }}{{ add 2 3 }}{{ end }}`),
	}
	set, err := web.LoadTemplates(fsys, "tpl", web.FuncMap{
		"add": func(a, b int) int { return 100 },
	})
	if err != nil {
		t.Fatal(err)
	}
	if out := render(t, set, "home", "layout"); !strings.Contains(out, "100") {
		t.Errorf("caller func should win; got %q", out)
	}
}

func TestLoad_VariadicFuncMapsMergeInOrder(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": file(baseLayout),
		"tpl/_default/home.html":    file(`{{ define "content" }}{{ tag }}{{ end }}`),
	}
	set, err := web.LoadTemplates(fsys, "tpl",
		web.FuncMap{"tag": func() string { return "first" }},
		nil,
		web.FuncMap{"tag": func() string { return "last" }},
	)
	if err != nil {
		t.Fatal(err)
	}
	if out := render(t, set, "home", "layout"); !strings.Contains(out, "last") {
		t.Errorf("later map should win; got %q", out)
	}
}

func TestLoad_MissingDir_Errors(t *testing.T) {
	if _, err := web.LoadTemplates(fstest.MapFS{}, "nope"); err == nil {
		t.Fatal("want an error for a missing directory")
	}
}

func TestLoad_NoTemplatesFound(t *testing.T) {
	fsys := fstest.MapFS{"tpl/notes/readme.txt": file("not html")}
	if _, err := web.LoadTemplates(fsys, "tpl"); err == nil {
		t.Fatal("want an error when nothing was parsed")
	}
}

func TestLoad_ParseErrorNamesTheFile(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": file(baseLayout),
		"tpl/_default/broken.html":  file(`{{ define "content" }}{{ .Unclosed `),
	}
	_, err := web.LoadTemplates(fsys, "tpl")
	if err == nil || !strings.Contains(err.Error(), "broken.html") {
		t.Fatalf("err = %v, want it to name broken.html", err)
	}
}

func TestRender_UnknownPage_Errors(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": file(baseLayout),
		"tpl/_default/home.html":    file(`{{ define "content" }}home{{ end }}`),
	}
	set, err := web.LoadTemplates(fsys, "tpl")
	if err != nil {
		t.Fatal(err)
	}
	if err := set.Render(io.Discard, "nope", "layout", nil); err == nil {
		t.Fatal("want an error for an unknown page")
	}
}

func TestRender_ExecError_NoPartialWrite(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": file(baseLayout),
		"tpl/_default/home.html":    file(`{{ define "content" }}before {{ .Boom.Nope }} after{{ end }}`),
	}
	set, err := web.LoadTemplates(fsys, "tpl")
	if err != nil {
		t.Fatal(err)
	}
	// Non-nil data: with nil, a bad field path is silently no output.
	var buf bytes.Buffer
	if err := set.Render(&buf, "home", "layout", struct{}{}); err == nil {
		t.Fatal("want an execution error")
	}
	if buf.Len() != 0 {
		t.Errorf("nothing should be written, got %q", buf.String())
	}
}

// Render takes an io.Writer, so it cannot set headers. What is observable is
// that it leaves whatever the caller already decided alone.
func TestRender_LeavesHeadersToTheCaller(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": file(baseLayout),
		"tpl/_default/home.html":    file(`{{ define "content" }}home{{ end }}`),
	}
	set, err := web.LoadTemplates(fsys, "tpl")
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "text/html; charset=utf-8")
	rec.Header().Set("HX-Retarget", "#somewhere")
	rec.WriteHeader(http.StatusUnprocessableEntity)

	if err := set.Render(rec, "home", "layout", nil); err != nil {
		t.Fatal(err)
	}
	if got := rec.Result().Header.Get("HX-Retarget"); got != "#somewhere" {
		t.Errorf("HX-Retarget = %q, want it untouched", got)
	}
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("code = %d, want the status the caller chose", rec.Code)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("nope") }

func TestRender_WriterError(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": file(baseLayout),
		"tpl/_default/home.html":    file(`{{ define "content" }}home{{ end }}`),
	}
	set, err := web.LoadTemplates(fsys, "tpl")
	if err != nil {
		t.Fatal(err)
	}
	if err := set.Render(failingWriter{}, "home", "layout", nil); err == nil {
		t.Fatal("want the writer error")
	}
}

func TestLoadSources_CrossSourceFallback(t *testing.T) {
	shared := fstest.MapFS{
		"tpl/_default/_layout.html": file(`{{ define "layout" }}SHARED {{ template "content" . }}{{ end }}`),
	}
	pages := fstest.MapFS{
		"tpl/public/status.html": file(`{{ define "content" }}status{{ end }}`),
	}
	set, err := web.LoadTemplateSources([]web.TemplateSource{
		{FS: shared, Dir: "tpl"},
		{FS: pages, Dir: "tpl"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out := render(t, set, "public/status", "layout"); !strings.Contains(out, "SHARED") {
		t.Errorf("render = %q", out)
	}
}

func TestLoadSources_SectionCollision(t *testing.T) {
	one := fstest.MapFS{"tpl/public/a.html": file(`{{ define "content" }}a{{ end }}`)}
	two := fstest.MapFS{"tpl/public/b.html": file(`{{ define "content" }}b{{ end }}`)}
	_, err := web.LoadTemplateSources([]web.TemplateSource{{FS: one, Dir: "tpl"}, {FS: two, Dir: "tpl"}})
	if err == nil || !strings.Contains(err.Error(), "public") {
		t.Fatalf("err = %v, want a collision naming the section", err)
	}
}

func TestLoadSources_NilFS(t *testing.T) {
	if _, err := web.LoadTemplateSources([]web.TemplateSource{{FS: nil, Dir: "tpl"}}); err == nil {
		t.Fatal("want an error for a nil filesystem")
	}
}

func TestLoad_InvalidFuncMapIsAnError(t *testing.T) {
	fsys := fstest.MapFS{"tpl/_default/home.html": file(`x`)}
	_, err := web.LoadTemplates(fsys, "tpl", web.FuncMap{"bad": 42})
	if err == nil || !strings.Contains(err.Error(), "FuncMap") {
		t.Fatalf("err = %v, want an invalid FuncMap error", err)
	}
}

func TestSections(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/home.html": file(`x`),
		"tpl/public/status.html": file(`x`),
		"tpl/assets/logo.svg":    file(`<svg/>`),
	}
	got, err := web.TemplateSections(fsys, "tpl")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"_default", "public"}) {
		t.Errorf("Sections = %v, want [_default public]", got)
	}
	if _, err := web.TemplateSections(fsys, "missing"); err == nil || !strings.Contains(err.Error(), "web: template sections:") {
		t.Fatalf("err = %v, want the web-prefixed missing-directory error", err)
	}
	if _, err := web.TemplateSections(nil, "tpl"); err == nil {
		t.Fatal("want an error for a nil filesystem")
	}
}

func TestLoad_NonHTMLFilesIgnored(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": file(baseLayout),
		"tpl/_default/home.html":    file(`{{ define "content" }}home{{ end }}`),
		"tpl/_default/notes.txt":    file(`ignored`),
		"tpl/_default/script.js":    file(`also ignored`),
	}
	set, err := web.LoadTemplates(fsys, "tpl")
	if err != nil {
		t.Fatal(err)
	}
	if out := render(t, set, "home", "layout"); !strings.Contains(out, "home") {
		t.Errorf("home render missing content: %q", out)
	}
	var buf bytes.Buffer
	for _, page := range []string{"notes", "script"} {
		if err := set.Render(&buf, page, "layout", nil); err == nil {
			t.Errorf("non-HTML file %q should not become a page", page)
		}
	}
}

// A key the payload does not carry is a mistake in the page, not an empty
// string. Struct payloads already fail on an unknown field; this covers the
// map case, which otherwise renders "<no value>" and looks fine.
func TestRender_MissingMapKeyIsAnError(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": file(baseLayout),
		"tpl/_default/home.html":    file(`{{ define "content" }}{{ .Missing }}{{ end }}`),
	}
	set, err := web.LoadTemplates(fsys, "tpl")
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	err = set.Render(&buf, "home", "layout", map[string]any{"Present": "yes"})
	if err == nil {
		t.Fatal("want an error for a key the payload does not carry")
	}
	if buf.Len() != 0 {
		t.Errorf("nothing should be written, got %q", buf.String())
	}

	// A key that is present still renders.
	if out := renderData(t, set, "home", "layout", map[string]any{"Missing": "here"}); !strings.Contains(out, "here") {
		t.Errorf("render = %q", out)
	}
}

func renderData(t *testing.T, set *web.Templates, page, define string, data any) string {
	t.Helper()
	var buf bytes.Buffer
	if err := set.Render(&buf, page, define, data); err != nil {
		t.Fatalf("Render(%q, %q): %v", page, define, err)
	}
	return buf.String()
}

// A page's regions are the pieces it offers by name. The document, its
// structural blocks, and the parsing names created for each file remain
// internal.
func TestRegions_OnlyDeclaredPiecesAreOffered(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": file(baseLayout),
		"tpl/dash/page.html": file(`
{{define "content"}}{{template "region/results" .}}{{template "admin-tools" .}}{{end}}
{{define "region/results"}}<section id="results">results</section>{{end}}
{{define "region/recent"}}<aside id="recent">recent</aside>{{end}}
{{define "admin-tools"}}<div>secrets</div>{{end}}
`),
	}
	set, err := web.LoadTemplates(fsys, "tpl", nil)
	if err != nil {
		t.Fatal(err)
	}

	regions := set.Regions("dash/page")
	want := []web.Region{
		{Name: "recent", Define: "region/recent"},
		{Name: "results", Define: "region/results"},
	}
	if !slices.Equal(regions, want) {
		t.Errorf("Regions() = %v, want %v", regions, want)
	}

	for _, tt := range []struct {
		name   string
		want   string
		wantOK bool
	}{
		{"results", "region/results", true},
		{"recent", "region/recent", true},
		{"admin-tools", "", false},
		{"content", "", false},
		{"layout", "", false},
		{"page.html", "", false},
		{"region/results", "", false},
		{"", "", false},
	} {
		define, ok := set.Region("dash/page", tt.name)
		if ok != tt.wantOK || define != tt.want {
			t.Errorf("Region(%q) = %q, %v; want %q, %v", tt.name, define, ok, tt.want, tt.wantOK)
		}
	}

	if _, ok := set.Region("no/such-page", "results"); ok {
		t.Error("an unknown page offers no regions")
	}

	if out := render(t, set, "dash/page", "region/results"); !strings.Contains(out, `id="results"`) {
		t.Errorf("region render = %q", out)
	}
}

// A region inherited by a page is part of that page's public region set,
// including regions from the default section.
func TestRegions_AreInherited(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": file(baseLayout),
		"tpl/_default/_flash.html":  file(`{{define "region/flash"}}<p id="flash">!</p>{{end}}`),
		"tpl/dash/page.html":        file(`{{define "content"}}x{{end}}`),
	}
	set, err := web.LoadTemplates(fsys, "tpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := set.Region("dash/page", "flash"); !ok {
		t.Error("a page that can see a region offers it")
	}
}

func TestRegions_UnnamedIsRejectedAtLoad(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": file(baseLayout),
		"tpl/dash/page.html": file(`
{{define "content"}}x{{end}}
{{define "region/"}}<p>nameless</p>{{end}}
`),
	}
	if _, err := web.LoadTemplates(fsys, "tpl", nil); err == nil {
		t.Fatal("a region with no name should fail the load")
	} else if !strings.Contains(err.Error(), "no name") {
		t.Errorf("error = %v", err)
	}
}

// The one convention Render carries: an empty define is the conventional
// layout partial's own file name.
func TestRender_EmptyDefineRendersLayout(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": file(`<html>{{ template "content" . }}</html>`),
		"tpl/_default/home.html":    file(`{{ define "content" }}<p>home</p>{{ end }}`),
	}
	set, err := web.LoadTemplates(fsys, "tpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := render(t, set, "home", "")
	if !strings.Contains(got, "<html><p>home</p></html>") {
		t.Errorf("render = %q, want the layout-wrapped page", got)
	}
}
