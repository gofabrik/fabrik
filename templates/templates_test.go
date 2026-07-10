package templates_test

import (
	"errors"
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
