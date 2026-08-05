package web_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/web"
)

// These integration tests exercise the loader and adapter together over
// HTTP: templates from a real tree and responses through a served handler.

func serveTemplates(t *testing.T, fn func(*web.Request) (web.Response, error)) *httptest.Server {
	t.Helper()
	set, err := web.LoadTemplates(os.DirFS("testdata"), "templates")
	if err != nil {
		t.Fatal(err)
	}
	adapter := web.NewAdapter(web.WithRenderer(set))
	srv := httptest.NewServer(adapter.Wrap(fn))
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(body)
}

func TestIntegration_DefaultEntryRendersTheLayout(t *testing.T) {
	srv := serveTemplates(t, func(req *web.Request) (web.Response, error) {
		return web.Template("site/hello", map[string]any{"Title": "Hello"}), nil
	})
	resp, body := get(t, srv.URL)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if !strings.Contains(body, "<html><body><h1>Hello</h1></body></html>") {
		t.Errorf("body = %q, want the layout-wrapped page", body)
	}
}

func TestIntegration_BlockRendersTheFragmentAlone(t *testing.T) {
	srv := serveTemplates(t, func(req *web.Request) (web.Response, error) {
		return web.Template("site/hello", map[string]any{"Name": "Ada"}).Block("greet"), nil
	})
	_, body := get(t, srv.URL)
	if strings.TrimSpace(body) != "<p>hi Ada</p>" {
		t.Errorf("body = %q, want the fragment alone", body)
	}
}

func TestIntegration_StatusAndHeadersSurviveTheRender(t *testing.T) {
	srv := serveTemplates(t, func(req *web.Request) (web.Response, error) {
		return web.Template("site/hello", map[string]any{"Title": "Nope"}).
			Status(http.StatusUnprocessableEntity).
			Header("X-Kind", "form"), nil
	})
	resp, body := get(t, srv.URL)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Kind"); got != "form" {
		t.Errorf("X-Kind = %q, want form", got)
	}
	if !strings.Contains(body, "<h1>Nope</h1>") {
		t.Errorf("body = %q, want the rendered page", body)
	}
}

func TestIntegration_FailedRenderReachesTheErrorHandlerClean(t *testing.T) {
	srv := serveTemplates(t, func(req *web.Request) (web.Response, error) {
		// missingkey=error fails this render midway through the layout.
		return web.Template("site/broken", map[string]any{}).Header("X-Kind", "page"), nil
	})
	resp, body := get(t, srv.URL)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if strings.Contains(body, "before") {
		t.Errorf("body = %q: partial render output leaked", body)
	}
	if got := resp.Header.Get("X-Kind"); got != "" {
		t.Errorf("X-Kind = %q: the failed response's header leaked", got)
	}
}

func TestIntegration_UnknownPageIsAnError(t *testing.T) {
	srv := serveTemplates(t, func(req *web.Request) (web.Response, error) {
		return web.Template("site/nope", nil), nil
	})
	resp, body := get(t, srv.URL)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if strings.Contains(body, "nope") {
		t.Errorf("body = %q: internal detail leaked to the client", body)
	}
}
