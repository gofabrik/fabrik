package templates_test

import (
	"bytes"
	"errors"
	htmltemplate "html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gofabrik/fabrik/templates"
)

const baseLayout = `<html><body>{{ template "content" . }}</body></html>`

func TestLoad_DefaultSection_BareKeys(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": &fstest.MapFile{Data: []byte(baseLayout)},
		"tpl/_default/home.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}<p>home</p>{{ end }}`)},
		"tpl/_default/about.html":   &fstest.MapFile{Data: []byte(`{{ define "content" }}<p>about</p>{{ end }}`)},
	}
	set, err := templates.Load(fsys, "tpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	names := set.Names()
	if len(names) != 2 || names[0] != "about" || names[1] != "home" {
		t.Errorf("Names() = %v, want [about home]", names)
	}
	rec := httptest.NewRecorder()
	if err := set.Render(rec, "home", nil); err != nil {
		t.Fatalf("Render(%s): %v", "home", err)
	}
	if !strings.Contains(rec.Body.String(), "<p>home</p>") {
		t.Errorf("home render missing content: %q", rec.Body.String())
	}
}

func TestLoad_SectionShadowsDefaultLayout(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": &fstest.MapFile{Data: []byte(`<html>DEFAULT {{ template "content" . }}</html>`)},
		"tpl/_default/home.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}home{{ end }}`)},
		"tpl/public/_layout.html":   &fstest.MapFile{Data: []byte(`<html>PUBLIC {{ template "content" . }}</html>`)},
		"tpl/public/status.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}status{{ end }}`)},
	}
	set, err := templates.Load(fsys, "tpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	if err := set.Render(rec, "home", nil); err != nil {
		t.Fatalf("Render(%s): %v", "home", err)
	}
	if !strings.Contains(rec.Body.String(), "DEFAULT") {
		t.Errorf("home should use _default layout; got %q", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	if err := set.Render(rec, "public/status", nil); err != nil {
		t.Fatalf("Render(public/status): %v", err)
	}
	if !strings.Contains(rec.Body.String(), "PUBLIC") {
		t.Errorf("public/status should use public layout; got %q", rec.Body.String())
	}
}

func TestLoad_SectionFallsBackToDefaultLayout(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": &fstest.MapFile{Data: []byte(`<html>FALLBACK {{ template "content" . }}</html>`)},
		"tpl/_default/home.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}home{{ end }}`)},
		"tpl/errors/404.html":       &fstest.MapFile{Data: []byte(`{{ define "content" }}not found{{ end }}`)},
	}
	set, err := templates.Load(fsys, "tpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	if err := set.Render(rec, "errors/404", nil); err != nil {
		t.Fatalf("Render(%s): %v", "errors/404", err)
	}
	got := rec.Body.String()
	if !strings.Contains(got, "FALLBACK") || !strings.Contains(got, "not found") {
		t.Errorf("errors/404 should pick up FALLBACK layout; got %q", got)
	}
}

func TestLoad_PartialsAreSharedFromDefault(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": &fstest.MapFile{Data: []byte(`<html>{{ template "_flash" . }}|{{ template "content" . }}</html>`)},
		"tpl/_default/_flash.html":  &fstest.MapFile{Data: []byte(`{{ define "_flash" }}FLASH{{ end }}`)},
		"tpl/_default/home.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}home{{ end }}`)},
		"tpl/public/_layout.html":   &fstest.MapFile{Data: []byte(`<html>{{ template "_flash" . }}|{{ template "content" . }}</html>`)},
		"tpl/public/status.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}status{{ end }}`)},
	}
	set, err := templates.Load(fsys, "tpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"home", "public/status"} {
		rec := httptest.NewRecorder()
		if err := set.Render(rec, name, nil); err != nil {
			t.Fatalf("Render(%s): %v", name, err)
		}
		if !strings.Contains(rec.Body.String(), "FLASH") {
			t.Errorf("%s missing _flash partial output: %q", name, rec.Body.String())
		}
	}
}

func TestLoad_SectionPartialShadowsDefault(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": &fstest.MapFile{Data: []byte(`<html>{{ template "_flash" . }}|{{ template "content" . }}</html>`)},
		"tpl/_default/_flash.html":  &fstest.MapFile{Data: []byte(`{{ define "_flash" }}DEFAULT-FLASH{{ end }}`)},
		"tpl/_default/home.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}home{{ end }}`)},
		"tpl/public/_flash.html":    &fstest.MapFile{Data: []byte(`{{ define "_flash" }}PUBLIC-FLASH{{ end }}`)},
		"tpl/public/status.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}status{{ end }}`)},
	}
	set, err := templates.Load(fsys, "tpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	if err := set.Render(rec, "home", nil); err != nil {
		t.Fatalf("Render(%s): %v", "home", err)
	}
	if !strings.Contains(rec.Body.String(), "DEFAULT-FLASH") {
		t.Errorf("home should see DEFAULT-FLASH; got %q", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	if err := set.Render(rec, "public/status", nil); err != nil {
		t.Fatalf("Render(public/status): %v", err)
	}
	if !strings.Contains(rec.Body.String(), "PUBLIC-FLASH") {
		t.Errorf("public/status should see PUBLIC-FLASH; got %q", rec.Body.String())
	}
}

func TestLoad_DefaultFuncsAvailable(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": &fstest.MapFile{Data: []byte(`{{ template "content" . }}`)},
		"tpl/_default/page.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}{{ add 2 3 }}{{ end }}`)},
	}
	set, err := templates.Load(fsys, "tpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	if err := set.Render(rec, "page", nil); err != nil {
		t.Fatalf("Render(%s): %v", "page", err)
	}
	if got := rec.Body.String(); !strings.Contains(got, "5") {
		t.Errorf("add 2 3 = %q, want contains 5", got)
	}
}

func TestLoad_UserFuncsShadowDefaults(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": &fstest.MapFile{Data: []byte(`{{ template "content" . }}`)},
		"tpl/_default/page.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}{{ add 2 3 }}{{ end }}`)},
	}
	set, err := templates.Load(fsys, "tpl", templates.FuncMap{
		"add": func(a, b int) int { return a * b },
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	if err := set.Render(rec, "page", nil); err != nil {
		t.Fatalf("Render(%s): %v", "page", err)
	}
	if got := rec.Body.String(); !strings.Contains(got, "6") {
		t.Errorf("user-supplied add should win; got %q", got)
	}
}

func TestLoad_VariadicFuncMapsMergeInOrder(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": &fstest.MapFile{Data: []byte(`{{ template "content" . }}`)},
		"tpl/_default/page.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}{{ greet }}|{{ asset "x" }}{{ end }}`)},
	}
	assetmapperLike := templates.FuncMap{
		"asset": func(s string) string { return "/A/" + s },
		"greet": func() string { return "lib-greet" },
	}
	custom := templates.FuncMap{
		"greet": func() string { return "custom-greet" },
	}
	set, err := templates.Load(fsys, "tpl", assetmapperLike, custom)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	if err := set.Render(rec, "page", nil); err != nil {
		t.Fatalf("Render(%s): %v", "page", err)
	}
	got := rec.Body.String()
	if !strings.Contains(got, "custom-greet") {
		t.Errorf("custom map should override earlier map on collision; got %q", got)
	}
	if !strings.Contains(got, "/A/x") {
		t.Errorf("earlier map's non-colliding entry should still apply; got %q", got)
	}
}

func TestLoad_NoFuncMaps_DefaultsStillAvailable(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": &fstest.MapFile{Data: []byte(`{{ template "content" . }}`)},
		"tpl/_default/page.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}{{ add 1 2 }}{{ end }}`)},
	}
	set, err := templates.Load(fsys, "tpl")
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	if err := set.Render(rec, "page", nil); err != nil {
		t.Fatalf("Render(%s): %v", "page", err)
	}
	if got := rec.Body.String(); !strings.Contains(got, "3") {
		t.Errorf("DefaultFuncs.add should still be available; got %q", got)
	}
}

func TestLoad_SectionWithoutLayoutAndNoFallback_Errors(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/only/home.html": &fstest.MapFile{Data: []byte(`{{ define "content" }}home{{ end }}`)},
	}
	if _, err := templates.Load(fsys, "tpl", nil); err == nil {
		t.Fatal("expected error for section with pages but no layout (and no _default)")
	}
}

func TestLoad_MissingDir_Errors(t *testing.T) {
	fsys := fstest.MapFS{}
	if _, err := templates.Load(fsys, "nope", nil); err == nil {
		t.Fatal("expected error for missing dir")
	}
}

func TestRender_UnknownPage_Errors(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": &fstest.MapFile{Data: []byte(baseLayout)},
		"tpl/_default/home.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}home{{ end }}`)},
	}
	set, err := templates.Load(fsys, "tpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	if err := set.Render(rec, "missing", nil); err == nil {
		t.Error("Render of unknown page = nil error, want error")
	}
	if rec.Body.Len() != 0 {
		t.Errorf("unknown page wrote %d bytes, want 0", rec.Body.Len())
	}
}

func TestRender_ExecError_NoPartialWrite(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": &fstest.MapFile{Data: []byte(baseLayout)},
		"tpl/_default/page.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}<p>{{ boom }}</p>{{ end }}`)},
	}
	funcs := templates.FuncMap{"boom": func() (string, error) { return "", errors.New("kaboom") }}
	set, err := templates.Load(fsys, "tpl", funcs)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	if err := set.Render(rec, "page", nil); err == nil {
		t.Error("Render with failing template = nil error, want error")
	}
	if rec.Body.Len() != 0 {
		t.Errorf("failed render wrote %d bytes, want 0 (buffer must not flush on error)", rec.Body.Len())
	}
}

func TestLoadSources_CrossSourceFallback(t *testing.T) {
	shared := fstest.MapFS{
		"templates/_default/_layout.html": &fstest.MapFile{Data: []byte(`layout[{{ block "content" . }}{{ end }}]`)},
		"templates/_default/_nav.html":    &fstest.MapFile{Data: []byte(`nav`)},
	}
	web := fstest.MapFS{
		"templates/web/home.html": &fstest.MapFile{Data: []byte(`{{ define "content" }}{{ template "_nav.html" . }}+home{{ end }}`)},
	}
	set, err := templates.LoadSources([]templates.Source{
		{FS: shared, Dir: "templates"},
		{FS: web, Dir: "templates"},
	})
	if err != nil {
		t.Fatalf("LoadSources: %v", err)
	}
	rec := httptest.NewRecorder()
	if err := set.Render(rec, "web/home", nil); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := rec.Body.String(); got != "layout[nav+home]" {
		t.Fatalf("web/home = %q, want cross-source layout and partial", got)
	}
}

func TestLoadSources_SectionCollision(t *testing.T) {
	a := fstest.MapFS{"templates/web/x.html": &fstest.MapFile{Data: []byte(`x`)}}
	b := fstest.MapFS{"templates/web/y.html": &fstest.MapFile{Data: []byte(`y`)}}
	_, err := templates.LoadSources([]templates.Source{
		{FS: a, Dir: "templates"},
		{FS: b, Dir: "templates"},
	})
	if err == nil || !strings.Contains(err.Error(), `section "web" comes from source 0`) {
		t.Fatalf("err = %v, want section collision", err)
	}
}

func TestLoadSources_DefaultCollision(t *testing.T) {
	a := fstest.MapFS{"templates/_default/_layout.html": &fstest.MapFile{Data: []byte(`a`)}}
	b := fstest.MapFS{"templates/_default/_layout.html": &fstest.MapFile{Data: []byte(`b`)}}
	_, err := templates.LoadSources([]templates.Source{
		{FS: a, Dir: "templates"},
		{FS: b, Dir: "templates"},
	})
	if err == nil || !strings.Contains(err.Error(), `section "_default" comes from source 0`) {
		t.Fatalf("err = %v, want _default collision", err)
	}
}

func TestLoadSources_NonTemplateDirsDoNotCollide(t *testing.T) {
	a := fstest.MapFS{
		"templates/_default/_layout.html": &fstest.MapFile{Data: []byte(`{{ block "content" . }}{{ end }}`)},
		"templates/_default/home.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}ok{{ end }}`)},
		"templates/assets/style.css":      &fstest.MapFile{Data: []byte(`body{}`)},
	}
	b := fstest.MapFS{
		"templates/web/index.html":   &fstest.MapFile{Data: []byte(`{{ define "content" }}web{{ end }}`)},
		"templates/assets/other.css": &fstest.MapFile{Data: []byte(`p{}`)},
	}
	set, err := templates.LoadSources([]templates.Source{
		{FS: a, Dir: "templates"},
		{FS: b, Dir: "templates"},
	})
	if err != nil {
		t.Fatalf("LoadSources: %v (asset dirs must not collide)", err)
	}
	if names := set.Names(); len(names) != 2 {
		t.Fatalf("Names() = %v, want [home web/index]", names)
	}
}

func TestSections_SkipsNonTemplateDirs(t *testing.T) {
	fsys := fstest.MapFS{
		"templates/_default/_layout.html": &fstest.MapFile{Data: []byte(`x`)},
		"templates/web/home.html":         &fstest.MapFile{Data: []byte(`x`)},
		"templates/assets/style.css":      &fstest.MapFile{Data: []byte(`body{}`)},
	}
	got, err := templates.Sections(fsys, "templates")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "_default" || got[1] != "web" {
		t.Fatalf("Sections = %v, want [_default web]", got)
	}
}

func TestLoad_InvalidFuncMapIsAnError(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": &fstest.MapFile{Data: []byte(`x`)},
		"tpl/_default/home.html":    &fstest.MapFile{Data: []byte(`x`)},
	}
	_, err := templates.Load(fsys, "tpl", templates.FuncMap{"x": 1})
	if err == nil || !strings.Contains(err.Error(), "invalid FuncMap") {
		t.Fatalf("err = %v, want invalid-FuncMap error, not a panic", err)
	}
}

func TestLoadSources_NilFS(t *testing.T) {
	_, err := templates.LoadSources([]templates.Source{{FS: nil, Dir: "templates"}})
	if err == nil || !strings.Contains(err.Error(), "nil filesystem") {
		t.Fatalf("err = %v, want nil-filesystem error", err)
	}
}

func TestSections_NilFS(t *testing.T) {
	if _, err := templates.Sections(nil, "templates"); err == nil {
		t.Fatal("want error for nil filesystem")
	}
}

func TestLoad_SectionDefineBeatsDefaultAcrossFilenames(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": &fstest.MapFile{Data: []byte(`[{{ template "nav" . }}|{{ block "content" . }}{{ end }}]`)},
		"tpl/_default/_nav.html":    &fstest.MapFile{Data: []byte(`{{ define "nav" }}DEFAULT{{ end }}`)},
		"tpl/public/_sidebar.html":  &fstest.MapFile{Data: []byte(`{{ define "nav" }}SECTION{{ end }}`)},
		"tpl/public/status.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}s{{ end }}`)},
		"tpl/_default/home.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}h{{ end }}`)},
	}
	set, err := templates.Load(fsys, "tpl")
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	if err := set.Render(rec, "public/status", nil); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := rec.Body.String(); got != "[SECTION|s]" {
		t.Fatalf("public/status = %q, want section-local nav to win", got)
	}
	rec = httptest.NewRecorder()
	if err := set.Render(rec, "home", nil); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := rec.Body.String(); got != "[DEFAULT|h]" {
		t.Fatalf("home = %q, want default nav in _default", got)
	}
}

func TestRender_PlainWriter(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": &fstest.MapFile{Data: []byte(baseLayout)},
		"tpl/_default/home.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}<p>hi</p>{{ end }}`)},
	}
	set, err := templates.Load(fsys, "tpl")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := set.Render(&buf, "home", nil); err != nil {
		t.Fatalf("Render into bytes.Buffer: %v", err)
	}
	if !strings.Contains(buf.String(), "<p>hi</p>") {
		t.Errorf("buffer = %q, want rendered content", buf.String())
	}
}

type headerSpy struct {
	bytes.Buffer
	header http.Header
}

func (h *headerSpy) Header() http.Header { return h.header }

func TestRender_SetsNoHeaders(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": &fstest.MapFile{Data: []byte(baseLayout)},
		"tpl/_default/home.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}x{{ end }}`)},
	}
	set, err := templates.Load(fsys, "tpl")
	if err != nil {
		t.Fatal(err)
	}
	spy := &headerSpy{header: http.Header{}}
	if err := set.Render(spy, "home", nil); err != nil {
		t.Fatal(err)
	}
	if len(spy.header) != 0 {
		t.Errorf("headers = %v; headers are the caller's concern, not the library's", spy.header)
	}
}

func TestRender_UnknownTemplateError(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": &fstest.MapFile{Data: []byte(baseLayout)},
		"tpl/_default/home.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}x{{ end }}`)},
	}
	set, err := templates.Load(fsys, "tpl")
	if err != nil {
		t.Fatal(err)
	}
	err = set.Render(io.Discard, "nope", nil)
	if err == nil || !strings.Contains(err.Error(), `unknown template "nope"`) {
		t.Errorf("err = %v, want unknown template", err)
	}
}

func textTree() fstest.MapFS {
	return fstest.MapFS{
		"tpl/mail/_layout.txt": &fstest.MapFile{Data: []byte("{{ template \"content\" . }}\n-- \nThe team")},
		"tpl/mail/welcome.txt": &fstest.MapFile{Data: []byte("Welcome, {{ .Name }}!")},
		"tpl/ops/robots.txt":   &fstest.MapFile{Data: []byte("Disallow: /admin for {{ .Name }}")},
	}
}

func TestLoad_TextTemplates_WrappedAndBare(t *testing.T) {
	set, err := templates.Load(textTree(), "tpl")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := set.Render(&buf, "mail/welcome.txt", map[string]any{"Name": "Ada"}); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); !strings.Contains(got, "Welcome, Ada!") || !strings.Contains(got, "The team") {
		t.Errorf("wrapped text = %q, want body wrapped by _layout.txt", got)
	}
	buf.Reset()
	if err := set.Render(&buf, "ops/robots.txt", map[string]any{"Name": "Ada"}); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "Disallow: /admin for Ada" {
		t.Errorf("bare text = %q, want the file as the whole body, no layout required", got)
	}
}

func TestLoad_TextNamesKeepExtension(t *testing.T) {
	set, err := templates.Load(textTree(), "tpl")
	if err != nil {
		t.Fatal(err)
	}
	names := set.Names()
	want := []string{"mail/welcome.txt", "ops/robots.txt"}
	for _, w := range want {
		found := false
		for _, n := range names {
			if n == w {
				found = true
			}
		}
		if !found {
			t.Errorf("Names() = %v, missing %q", names, w)
		}
	}
}

func TestLoad_TextEscapingIsRaw(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/mail/pair.txt":     &fstest.MapFile{Data: []byte("{{ .V }}")},
		"tpl/mail/_layout.html": &fstest.MapFile{Data: []byte(baseLayout)},
		"tpl/mail/pair.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}{{ .V }}{{ end }}`)},
	}
	set, err := templates.Load(fsys, "tpl")
	if err != nil {
		t.Fatal(err)
	}
	data := map[string]any{"V": "a & b"}
	var buf bytes.Buffer
	if err := set.Render(&buf, "mail/pair.txt", data); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "a & b" {
		t.Errorf("text = %q, want raw ampersand", buf.String())
	}
	buf.Reset()
	if err := set.Render(&buf, "mail/pair", data); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "a &amp; b") {
		t.Errorf("html = %q, want escaped ampersand", buf.String())
	}
}

func TestLoad_TextDefaultFallbackPerPlane(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.txt": &fstest.MapFile{Data: []byte("D:{{ template \"content\" . }}")},
		"tpl/_default/_sig.txt":    &fstest.MapFile{Data: []byte("{{ define \"sig\" }}default-sig{{ end }}")},
		"tpl/mail/reset.txt":       &fstest.MapFile{Data: []byte("body {{ template \"sig\" . }}")},
	}
	set, err := templates.Load(fsys, "tpl")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := set.Render(&buf, "mail/reset.txt", nil); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "D:body default-sig" {
		t.Errorf("text with _default fallback = %q", got)
	}
}

func TestLoad_TextPartialShadowing(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.txt": &fstest.MapFile{Data: []byte("{{ template \"content\" . }}")},
		"tpl/_default/_sig.txt":    &fstest.MapFile{Data: []byte("{{ define \"sig\" }}default-sig{{ end }}")},
		"tpl/mail/_sig.txt":        &fstest.MapFile{Data: []byte("{{ define \"sig\" }}local-sig{{ end }}")},
		"tpl/mail/reset.txt":       &fstest.MapFile{Data: []byte("{{ template \"sig\" . }}")},
	}
	set, err := templates.Load(fsys, "tpl")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := set.Render(&buf, "mail/reset.txt", nil); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "local-sig" {
		t.Errorf("shadowed partial = %q, want the section-local definition", buf.String())
	}
}

func TestLoad_TextBodyDefinesRejected(t *testing.T) {
	cases := map[string]string{
		"define-only":      `{{ define "content" }}HIJACK{{ end }}`,
		"define with text": "x\n{{ define \"content\" }}HIJACK{{ end }}",
		"other name":       `x{{ define "other" }}y{{ end }}`,
		"block":            `x{{ block "b" . }}y{{ end }}`,
	}
	for name, body := range cases {
		fsys := fstest.MapFS{
			"tpl/ops/page.txt": &fstest.MapFile{Data: []byte(body)},
		}
		_, err := templates.Load(fsys, "tpl")
		if err == nil || !(strings.Contains(err.Error(), "raw bodies") || strings.Contains(err.Error(), "multiple definition")) {
			t.Errorf("%s: err = %v, want body-definition rejection", name, err)
		}
	}
}

func TestLoad_TextBuiltinsAllowed(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/ops/page.txt": &fstest.MapFile{Data: []byte(`{{ printf "%s!" .Name }}`)},
	}
	set, err := templates.Load(fsys, "tpl")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := set.Render(&buf, "ops/page.txt", map[string]any{"Name": "Ada"}); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "Ada!" {
		t.Errorf("builtin call = %q", buf.String())
	}
}

func TestLoad_LayoutAdditionKeepsBodySyntax(t *testing.T) {
	body := "Reset for {{ .Name }}"
	bare := fstest.MapFS{
		"tpl/mail/reset.txt": &fstest.MapFile{Data: []byte(body)},
	}
	wrapped := fstest.MapFS{
		"tpl/mail/_layout.txt": &fstest.MapFile{Data: []byte("W:{{ template \"content\" . }}")},
		"tpl/mail/reset.txt":   &fstest.MapFile{Data: []byte(body)},
	}
	data := map[string]any{"Name": "Ada"}
	var out [2]string
	for i, fsys := range []fstest.MapFS{bare, wrapped} {
		set, err := templates.Load(fsys, "tpl")
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if err := set.Render(&buf, "mail/reset.txt", data); err != nil {
			t.Fatal(err)
		}
		out[i] = buf.String()
	}
	if out[0] != "Reset for Ada" || out[1] != "W:Reset for Ada" {
		t.Errorf("bare = %q wrapped = %q; the identical file must work in both modes", out[0], out[1])
	}
}

func TestLoad_CrossPlaneNameCollisionFails(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/mail/_layout.html": &fstest.MapFile{Data: []byte(baseLayout)},
		"tpl/mail/foo.txt.html": &fstest.MapFile{Data: []byte(`{{ define "content" }}html{{ end }}`)},
		"tpl/mail/foo.txt":      &fstest.MapFile{Data: []byte("text")},
	}
	_, err := templates.Load(fsys, "tpl")
	if err == nil || !strings.Contains(err.Error(), "duplicate template") {
		t.Errorf("err = %v, want the cross-plane duplicate to fail loudly", err)
	}
}

func TestLoad_TextOnlyDirectoryIsASection(t *testing.T) {
	set, err := templates.Load(fstest.MapFS{
		"tpl/ops/note.txt": &fstest.MapFile{Data: []byte("hello")},
	}, "tpl")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := set.Render(&buf, "ops/note.txt", nil); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "hello" {
		t.Errorf("text-only section render = %q", buf.String())
	}
}

func TestLoad_RootLevelTxtIgnored(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/note.txt":              &fstest.MapFile{Data: []byte("not a template")},
		"tpl/_default/_layout.html": &fstest.MapFile{Data: []byte(baseLayout)},
		"tpl/_default/home.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}x{{ end }}`)},
	}
	set, err := templates.Load(fsys, "tpl")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range set.Names() {
		if n == "note.txt" {
			t.Errorf("root-level .txt must stay outside section discovery; Names() = %v", set.Names())
		}
	}
}

func TestRender_TextExecErrorWritesNothing(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/ops/boom.txt": &fstest.MapFile{Data: []byte(`{{ boom }}`)},
	}
	set, err := templates.Load(fsys, "tpl", templates.FuncMap{
		"boom": func() (string, error) { return "", errors.New("kaboom") },
	})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := set.Render(&buf, "ops/boom.txt", nil); err == nil {
		t.Fatal("want execution error")
	}
	if buf.Len() != 0 {
		t.Errorf("failed render wrote %d bytes, want 0", buf.Len())
	}
}

func TestLoad_HTMLTypedFuncPrintsRawInText(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/ops/page.txt": &fstest.MapFile{Data: []byte(`{{ widget }}`)},
	}
	set, err := templates.Load(fsys, "tpl", templates.FuncMap{
		"widget": func() htmltemplate.HTML { return "<b>w</b>" },
	})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := set.Render(&buf, "ops/page.txt", nil); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "<b>w</b>" {
		t.Errorf("text output = %q; funcs reach both planes as passed, markup prints raw", buf.String())
	}
}
