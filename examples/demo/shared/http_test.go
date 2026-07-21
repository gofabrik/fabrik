package shared

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gofabrik/fabrik/templates"
)

const errLayout = `{{ template "content" . }}`

func errorSet(t *testing.T, files map[string]string) *templates.Set {
	t.Helper()
	fsys := fstest.MapFS{"tpl/_default/_layout.html": &fstest.MapFile{Data: []byte(errLayout)}}
	for name, body := range files {
		fsys["tpl/"+name] = &fstest.MapFile{Data: []byte(body)}
	}
	set, err := templates.Load(fsys, "tpl")
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func TestErrorPages_RenderSuccess(t *testing.T) {
	pages := &ErrorPages{Templates: errorSet(t, map[string]string{
		"errors/404.html": `{{ define "content" }}missing: {{ .Path }}{{ end }}`,
		"errors/405.html": `{{ define "content" }}no {{ .Method }}{{ end }}`,
	})}

	rec := httptest.NewRecorder()
	pages.NotFound(rec, httptest.NewRequest("GET", "/nope", nil))
	if !strings.Contains(rec.Body.String(), "missing: /nope") {
		t.Errorf("404 body = %q, want the rendered template", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("404 Content-Type = %q, want the handler-set HTML type", ct)
	}

	rec = httptest.NewRecorder()
	pages.MethodNotAllowed(rec, httptest.NewRequest("DELETE", "/", nil))
	if !strings.Contains(rec.Body.String(), "no DELETE") {
		t.Errorf("405 body = %q, want the rendered template", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("405 Content-Type = %q, want the handler-set HTML type", ct)
	}
}

func TestErrorPages_RenderFailureFallsBack(t *testing.T) {
	pages := &ErrorPages{Templates: errorSet(t, map[string]string{
		"misc/home.html": `{{ define "content" }}x{{ end }}`,
	})}

	rec := httptest.NewRecorder()
	pages.NotFound(rec, httptest.NewRequest("GET", "/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("fallback 404 status = %d", rec.Code)
	}
	if strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
		t.Errorf("fallback Content-Type = %q, want no leaked HTML header", rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(rec.Body.String(), "404 page not found") {
		t.Errorf("fallback body = %q, want http.NotFound's response", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	pages.MethodNotAllowed(rec, httptest.NewRequest("DELETE", "/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("fallback 405 status = %d", rec.Code)
	}
	if strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
		t.Errorf("fallback Content-Type = %q, want no leaked HTML header", rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(rec.Body.String(), "method not allowed") {
		t.Errorf("fallback body = %q, want http.Error's response", rec.Body.String())
	}
}
