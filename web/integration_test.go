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

// These integration tests exercise the loader and adapter together over HTTP:
// templates from a real tree and responses through a served handler.

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
	return getWith(t, url, nil)
}

func getWith(t *testing.T, url string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
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

// varyFields reads the Vary field names off a response as a set, so a test
// compares names rather than searching one string for another.
func varyFields(resp *http.Response) map[string]bool {
	got := map[string]bool{}
	for _, line := range resp.Header.Values("Vary") {
		for _, name := range strings.Split(line, ",") {
			got[strings.ToLower(strings.TrimSpace(name))] = true
		}
	}
	return got
}

func assertSelectionVary(t *testing.T, resp *http.Response) {
	t.Helper()
	got := varyFields(resp)
	for _, name := range []string{"HX-Request", "HX-Boosted", "HX-History-Restore-Request", "HX-Target"} {
		if !got[strings.ToLower(name)] {
			t.Errorf("Vary = %q, missing %s", resp.Header.Values("Vary"), name)
		}
	}
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

// Fragment selection is exercised through real templates and a real adapter:
// each request kind's representation and the selection metadata on both page
// and region responses.

func serveFragmentPage(t *testing.T) *httptest.Server {
	t.Helper()
	return serveTemplates(t, func(req *web.Request) (web.Response, error) {
		return web.Template("dash/page", map[string]any{"Query": "todo"}).Fragment(), nil
	})
}

func TestIntegration_FragmentServesTheWholePageForAPlainGET(t *testing.T) {
	srv := serveFragmentPage(t)
	resp, body := get(t, srv.URL)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, "<!doctype html>") {
		t.Errorf("body = %q, want the whole document", body)
	}
	if !strings.Contains(body, `<div id="results">results for todo</div>`) || !strings.Contains(body, `<ul id="recent">`) {
		t.Errorf("body = %q, want both regions in the page", body)
	}
	assertSelectionVary(t, resp)
}

func TestIntegration_FragmentServesTheRegionForAnHtmxSwap(t *testing.T) {
	srv := serveFragmentPage(t)
	resp, body := getWith(t, srv.URL, map[string]string{"HX-Request": "true", "HX-Target": "results"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if strings.Contains(body, "<!doctype html>") {
		t.Errorf("body = %q, want the region alone without a document", body)
	}
	if strings.TrimSpace(body) != `<div id="results">results for todo</div>` {
		t.Errorf("body = %q, want the region body alone", body)
	}
	assertSelectionVary(t, resp)
}

func TestIntegration_FragmentServesTheWholePageForABoostedNavigation(t *testing.T) {
	srv := serveFragmentPage(t)
	resp, body := getWith(t, srv.URL, map[string]string{"HX-Request": "true", "HX-Boosted": "true", "HX-Target": "results"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, "<!doctype html>") {
		t.Errorf("body = %q, want the whole document for a boosted navigation", body)
	}
}

func TestIntegration_FragmentWithoutTargetIsRefusedAndNotStored(t *testing.T) {
	srv := serveFragmentPage(t)
	resp, _ := getWith(t, srv.URL, map[string]string{"HX-Request": "true"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

func TestIntegration_FragmentUnknownRegionIsRefusedAndNotStored(t *testing.T) {
	srv := serveFragmentPage(t)
	resp, _ := getWith(t, srv.URL, map[string]string{"HX-Request": "true", "HX-Target": "nope"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

func TestIntegration_FragmentMergesAnExistingVary(t *testing.T) {
	srv := serveTemplates(t, func(req *web.Request) (web.Response, error) {
		req.SetHeader("Vary", "Accept-Language")
		return web.Template("dash/page", map[string]any{"Query": "todo"}).Fragment(), nil
	})
	for name, headers := range map[string]map[string]string{
		"page": nil,
		"swap": {"HX-Request": "true", "HX-Target": "results"},
	} {
		resp, _ := getWith(t, srv.URL, headers)
		got := varyFields(resp)
		if !got["accept-language"] {
			t.Errorf("%s: Vary = %q, lost the handler's own field", name, resp.Header.Values("Vary"))
		}
		assertSelectionVary(t, resp)
	}
}
