package web_test

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gofabrik/fabrik/web"
)

func greetingSet(t *testing.T, funcMaps ...web.FuncMap) *web.Templates {
	t.Helper()
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": file(baseLayout),
		"tpl/_default/home.html":    file(`{{ define "content" }}<p>{{ greet }}</p>{{ end }}`),
	}
	set, err := web.LoadTemplates(fsys, "tpl", funcMaps...)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func TestRequestFuncs_StubFailsARenderWithoutAnOverlay(t *testing.T) {
	rf := web.RequestFuncs{
		"greet": func(r *http.Request) any { return func() string { return "hi" } },
	}
	set := greetingSet(t, rf.Stubs())

	var buf bytes.Buffer
	err := set.Render(&buf, "home", "layout", nil)
	if err == nil {
		t.Fatal("a stub rendered without an overlay should fail")
	}
	if !strings.Contains(err.Error(), "needs a request") {
		t.Errorf("error = %v", err)
	}
}

func TestRequestFuncs_ForOverlaysTheStub(t *testing.T) {
	rf := web.RequestFuncs{
		"greet": func(r *http.Request) any {
			return func() string { return "from " + r.URL.Path }
		},
	}
	set := greetingSet(t, rf.Stubs())

	req := httptest.NewRequest("GET", "/here", nil)
	var buf bytes.Buffer
	if err := set.RenderFuncs(&buf, "home", "layout", nil, rf.For(req)); err != nil {
		t.Fatalf("RenderFuncs: %v", err)
	}
	if !strings.Contains(buf.String(), "<p>from /here</p>") {
		t.Errorf("render = %q, want the request-bound value", buf.String())
	}
}

func TestRequestFuncs_NilConstructorFailsTheRender(t *testing.T) {
	rf := web.RequestFuncs{"greet": nil}
	set := greetingSet(t, rf.Stubs())

	var buf bytes.Buffer
	err := set.RenderFuncs(&buf, "home", "layout", nil, rf.For(httptest.NewRequest("GET", "/", nil)))
	if err == nil {
		t.Fatal("a nil constructor should fail the render")
	}
	if !strings.Contains(err.Error(), "nil constructor") {
		t.Errorf("error = %v", err)
	}
}

func TestMergeRequestFuncs_LaterWins(t *testing.T) {
	first := web.RequestFuncs{
		"greet": func(r *http.Request) any { return func() string { return "first" } },
	}
	second := web.RequestFuncs{
		"greet": func(r *http.Request) any { return func() string { return "second" } },
	}
	rf := web.MergeRequestFuncs(first, second)
	set := greetingSet(t, rf.Stubs())

	var buf bytes.Buffer
	if err := set.RenderFuncs(&buf, "home", "layout", nil, rf.For(httptest.NewRequest("GET", "/", nil))); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "second") {
		t.Errorf("render = %q, want the later map's func", buf.String())
	}
}

func TestDefaultRequestFuncs_ExposeTheRequest(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": file(baseLayout),
		"tpl/_default/home.html": file(`{{ define "content" }}` +
			`<p>{{ request.Method }} {{ request.Path }} {{ request.Host }}</p>` +
			`<p>id={{ pathValue "id" }} q={{ query "q" }}</p>{{ end }}`),
	}
	rf := web.DefaultRequestFuncs()
	set, err := web.LoadTemplates(fsys, "tpl", rf.Stubs())
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "http://demo.test/items/7?q=abc", nil)
	req.SetPathValue("id", "7")
	var buf bytes.Buffer
	if err := set.RenderFuncs(&buf, "home", "layout", nil, rf.For(req)); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	for _, want := range []string{"GET /items/7 demo.test", "id=7", "q=abc"} {
		if !strings.Contains(got, want) {
			t.Errorf("render = %q, missing %q", got, want)
		}
	}
}

func TestLoad_StaticAndRequestNamesCollide(t *testing.T) {
	rf := web.RequestFuncs{
		"greet": func(r *http.Request) any { return func() string { return "hi" } },
	}
	static := web.FuncMap{"greet": func() string { return "static" }}

	for name, maps := range map[string][]web.FuncMap{
		"stub after static": {static, rf.Stubs()},
		"static after stub": {rf.Stubs(), static},
	} {
		fsys := fstest.MapFS{
			"tpl/_default/_layout.html": file(baseLayout),
			"tpl/_default/home.html":    file(`{{ define "content" }}{{ greet }}{{ end }}`),
		}
		_, err := web.LoadTemplates(fsys, "tpl", maps...)
		if err == nil {
			t.Fatalf("%s: colliding static and request names should fail the load", name)
		}
		if !strings.Contains(err.Error(), "both a static helper and a request func") {
			t.Errorf("%s: error = %v", name, err)
		}
	}
}

func TestLoad_StaticOverStaticKeepsLaterWins(t *testing.T) {
	set := greetingSet(t,
		web.FuncMap{"greet": func() string { return "first" }},
		web.FuncMap{"greet": func() string { return "second" }},
	)
	if got := render(t, set, "home", "layout"); !strings.Contains(got, "second") {
		t.Errorf("render = %q, want the later static func", got)
	}
}

func TestAdapter_AppliesRequestFuncsToTemplateResponses(t *testing.T) {
	rf := web.RequestFuncs{
		"greet": func(r *http.Request) any {
			return func() string { return "for " + r.URL.Query().Get("name") }
		},
	}
	set := greetingSet(t, rf.Stubs())
	a := web.NewAdapter(web.WithRenderer(set), web.WithRequestFuncs(rf), web.WithBlock("layout"))

	h := a.Wrap(func(req *web.Request) (web.Response, error) {
		return web.Template("home", nil), nil
	})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/?name=Ada", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "for Ada") {
		t.Errorf("body = %q, want the request-bound value", rec.Body.String())
	}
}

func TestAdapter_RequestFuncsOnFragmentSwaps(t *testing.T) {
	rf := web.RequestFuncs{
		"greet": func(r *http.Request) any {
			return func() string { return "swapped " + r.URL.Query().Get("name") }
		},
	}
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": file(baseLayout),
		"tpl/_default/home.html": file(`{{ define "content" }}{{ template "region/hello" . }}{{ end }}` +
			`{{ define "region/hello" }}<span>{{ greet }}</span>{{ end }}`),
	}
	set, err := web.LoadTemplates(fsys, "tpl", rf.Stubs())
	if err != nil {
		t.Fatal(err)
	}
	a := web.NewAdapter(web.WithRenderer(set), web.WithRequestFuncs(rf), web.WithBlock("layout"))
	h := a.Wrap(func(req *web.Request) (web.Response, error) {
		return web.Template("home", nil).Fragment(), nil
	})

	req := httptest.NewRequest("GET", "/?name=Ada", nil)
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Target", "hello")
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "swapped Ada") {
		t.Errorf("body = %q, want the request-bound fragment", rec.Body.String())
	}
}

type noFuncsRenderer struct{}

func (noFuncsRenderer) Render(w io.Writer, name, block string, data any) error { return nil }

func TestAdapter_RequestFuncsNeedAFuncsRenderer(t *testing.T) {
	rf := web.RequestFuncs{
		"greet": func(r *http.Request) any { return func() string { return "hi" } },
	}
	var seen error
	a := web.NewAdapter(
		web.WithRenderer(noFuncsRenderer{}),
		web.WithRequestFuncs(rf),
		web.WithErrorHandler(func(w http.ResponseWriter, r *http.Request, err error) { seen = err }),
	)
	h := a.Wrap(func(req *web.Request) (web.Response, error) {
		return web.Template("home", nil), nil
	})
	h(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))

	if !errors.Is(seen, web.ErrRendererNoFuncs) {
		t.Fatalf("error = %v, want ErrRendererNoFuncs", seen)
	}
}

func TestRequestRenderer_RendersOutsideTheAdapter(t *testing.T) {
	rf := web.RequestFuncs{
		"greet": func(r *http.Request) any {
			return func() string { return "direct " + r.URL.Query().Get("name") }
		},
	}
	rr := web.RequestRenderer{Templates: greetingSet(t, rf.Stubs()), Funcs: rf}

	var buf bytes.Buffer
	if err := rr.Render(&buf, httptest.NewRequest("GET", "/?name=Ada", nil), "home", "layout", nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "direct Ada") {
		t.Errorf("render = %q, want the request-bound value", buf.String())
	}
}

func TestRequestFuncs_EmptyForIsNil(t *testing.T) {
	if got := (web.RequestFuncs{}).For(httptest.NewRequest("GET", "/", nil)); got != nil {
		t.Fatalf("For on an empty RequestFuncs = %v, want nil", got)
	}
}

func TestAdapter_EmptyRequestFuncsKeepThePlainRenderPath(t *testing.T) {
	a := web.NewAdapter(
		web.WithRenderer(noFuncsRenderer{}),
		web.WithRequestFuncs(web.RequestFuncs{}),
		web.WithErrorHandler(func(w http.ResponseWriter, r *http.Request, err error) {
			t.Fatalf("render error: %v", err)
		}),
	)
	h := a.Wrap(func(req *web.Request) (web.Response, error) {
		return web.Template("home", nil), nil
	})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestRequestFuncs_NonFuncConstructorResultFailsTheRender(t *testing.T) {
	rf := web.RequestFuncs{
		"greet": func(r *http.Request) any { return "not a function" },
	}
	set := greetingSet(t, rf.Stubs())

	var buf bytes.Buffer
	err := set.RenderFuncs(&buf, "home", "layout", nil, rf.For(httptest.NewRequest("GET", "/", nil)))
	if err == nil {
		t.Fatal("a constructor returning a non-function should fail the render")
	}
	if !strings.Contains(err.Error(), "not a function") {
		t.Errorf("error = %v", err)
	}
}
