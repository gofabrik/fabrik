package directive

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gofabrik/fabrik/router"
	"github.com/gofabrik/fabrik/web"
)

// The emitted composition: adapter-wrapped typed handlers on the router's
// miss hooks. A response that sets no status keeps the routing status, an
// explicit one wins, and the Allow header survives.
func TestTypedHooksThroughTheRouter(t *testing.T) {
	set, err := web.LoadTemplates(fstest.MapFS{
		"tpl/errors/404.html": &fstest.MapFile{Data: []byte(`{{ define "_layout.html" }}missing {{ .Path }}{{ end }}`)},
		"tpl/errors/410.html": &fstest.MapFile{Data: []byte(`{{ define "_layout.html" }}gone{{ end }}`)},
		"tpl/errors/405.html": &fstest.MapFile{Data: []byte(`{{ define "_layout.html" }}not allowed{{ end }}`)},
	}, "tpl")
	if err != nil {
		t.Fatal(err)
	}
	adapter := web.NewAdapter(web.WithRenderer(set))

	r := router.New()
	r.Get("/a", func(w http.ResponseWriter, _ *http.Request) {})
	r.NotFound(adapter.Wrap(func(req *web.Request) (web.Response, error) {
		if strings.HasPrefix(req.HTTP().URL.Path, "/gone") {
			return web.Template("errors/410", nil).Status(http.StatusGone), nil
		}
		return web.Template("errors/404", map[string]any{"Path": req.HTTP().URL.Path}), nil
	}))
	r.MethodNotAllowed(adapter.Wrap(func(req *web.Request) (web.Response, error) {
		return web.Template("errors/405", nil), nil
	}))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/nope", nil))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "missing /nope") {
		t.Fatalf("zero-status hook = %d %q, want the routing 404 with the rendered page", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/gone/x", nil))
	if rec.Code != http.StatusGone || !strings.Contains(rec.Body.String(), "gone") {
		t.Fatalf("explicit status = %d %q, want 410", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST", "/a", nil))
	if rec.Code != http.StatusMethodNotAllowed || !strings.Contains(rec.Body.String(), "not allowed") {
		t.Fatalf("method mismatch = %d %q, want the routing 405 with the rendered page", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Allow") == "" {
		t.Fatal("the Allow header did not survive the typed hook")
	}
}
