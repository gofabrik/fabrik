package web_test

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/web"
)

// regionRenderer is a renderer whose pages declare regions, like the one a
// template set provides. Only "results" and "recent" are addressable; "layout"
// and "content" exist but are not regions, which is the distinction the whole
// mechanism rests on.
type regionRenderer struct {
	name, block string
	calls       int
}

func (r *regionRenderer) Render(w io.Writer, name, block string, data any) error {
	r.name, r.block, r.calls = name, block, r.calls+1
	_, err := fmt.Fprintf(w, "rendered %s/%s", name, block)
	return err
}

func (r *regionRenderer) Region(page, name string) (string, bool) {
	switch {
	case page != "dash/page":
		return "", false
	case name == "results", name == "recent":
		return "region/" + name, true
	default:
		return "", false
	}
}

// plainRenderer cannot resolve regions.
type plainRenderer struct{}

func (plainRenderer) Render(w io.Writer, name, block string, data any) error {
	_, err := io.WriteString(w, "rendered")
	return err
}

func fragmentAdapter(r web.Renderer) *web.Adapter {
	return web.NewAdapter(web.WithRenderer(r), web.WithBlock("layout"))
}

func fragmentRequest(headers map[string]string) *http.Request {
	req := httptest.NewRequest("GET", "/dash", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

func serveFragment(a *web.Adapter, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	a.Wrap(func(*web.Request) (web.Response, error) {
		return web.Template("dash/page", nil).Fragment(), nil
	})(rec, req)
	return rec
}

// Which representation each kind of request gets. A boosted navigation and a
// history restoration carry HX-Request and still want the document, so a
// target on either of them is not a reason to send a piece of one.
func TestFragmentSelectsTheRepresentation(t *testing.T) {
	for _, tt := range []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"ordinary request", nil, "layout"},
		{"htmx swap", map[string]string{"HX-Request": "true", "HX-Target": "results"}, "region/results"},
		{"another region", map[string]string{"HX-Request": "true", "HX-Target": "recent"}, "region/recent"},
		{"boosted navigation", map[string]string{"HX-Request": "true", "HX-Boosted": "true", "HX-Target": "results"}, "layout"},
		{"history restore", map[string]string{"HX-Request": "true", "HX-History-Restore-Request": "true", "HX-Target": "results"}, "layout"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := &regionRenderer{}
			rec := serveFragment(fragmentAdapter(r), fragmentRequest(tt.headers))

			if rec.Code != http.StatusOK {
				t.Fatalf("code = %d, body %q", rec.Code, rec.Body)
			}
			if r.block != tt.want {
				t.Errorf("rendered %q, want %q", r.block, tt.want)
			}
		})
	}
}

// htmx sends HX-Target as the target element's id and omits it when there is
// none, so this is an ordinary swap aimed at an unidentified element. It has
// to fail: the alternative is swapping a whole document into it.
func TestFragmentWithoutTargetIsRefused(t *testing.T) {
	r := &regionRenderer{}
	rec := serveFragment(fragmentAdapter(r), fragmentRequest(map[string]string{"HX-Request": "true"}))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", rec.Code)
	}
	if r.calls != 0 {
		t.Errorf("renderer ran %d times, want none", r.calls)
	}
}

// A target naming something the page does not offer as a region is a 404, and
// nothing renders - including when it names a real template that is not one.
func TestFragmentRefusesUndeclaredTargets(t *testing.T) {
	for _, target := range []string{"layout", "content", "list.html", "_row.html", "nonsense", "region/results"} {
		t.Run(target, func(t *testing.T) {
			r := &regionRenderer{}
			rec := serveFragment(fragmentAdapter(r), fragmentRequest(map[string]string{
				"HX-Request": "true",
				"HX-Target":  target,
			}))

			if rec.Code != http.StatusNotFound {
				t.Errorf("code = %d, want 404", rec.Code)
			}
			if r.calls != 0 {
				t.Errorf("renderer ran %d times for %q, want none", r.calls, target)
			}
		})
	}
}

// One URL answering with either a page or a piece of one has to say so, or
// something between here and the browser will serve one for the other.
func TestFragmentVaries(t *testing.T) {
	for _, tt := range []struct {
		name    string
		headers map[string]string
	}{
		{"page response", nil},
		{"region response", map[string]string{"HX-Request": "true", "HX-Target": "results"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := serveFragment(fragmentAdapter(&regionRenderer{}), fragmentRequest(tt.headers))
			assertVary(t, rec, "HX-Request", "HX-Boosted", "HX-History-Restore-Request", "HX-Target")
		})
	}
}

// A response carrying a Vary of its own must not lose the fields the
// selection depends on: response headers are applied with Set, so a second
// Vary would replace these rather than join them.
func TestFragmentVaryJoinsTheResponses(t *testing.T) {
	rec := httptest.NewRecorder()
	fragmentAdapter(&regionRenderer{}).Wrap(func(*web.Request) (web.Response, error) {
		return web.Template("dash/page", nil).Fragment().Header("Vary", "Accept-Language"), nil
	})(rec, fragmentRequest(nil))

	assertVary(t, rec, "Accept-Language", "HX-Request", "HX-Boosted", "HX-History-Restore-Request", "HX-Target")
}

// A response that cannot be reused at all says so once, rather than growing a
// field list that contradicts it.
func TestFragmentVaryLeavesTheWildcardAlone(t *testing.T) {
	rec := httptest.NewRecorder()
	fragmentAdapter(&regionRenderer{}).Wrap(func(*web.Request) (web.Response, error) {
		return web.Template("dash/page", nil).Fragment().Header("Vary", "*"), nil
	})(rec, fragmentRequest(nil))

	if got := rec.Header().Get("Vary"); got != "*" {
		t.Errorf("Vary = %q, want *", got)
	}
}

// The refusals leave through the error handler, which puts the header map
// back, so they cannot carry the Vary. They have to be uncacheable instead, or
// a shared cache could answer an ordinary request with one of them.
func TestFragmentRefusalsAreNotStored(t *testing.T) {
	for _, tt := range []struct {
		name    string
		headers map[string]string
		code    int
	}{
		{"no target", map[string]string{"HX-Request": "true"}, http.StatusBadRequest},
		{"unknown target", map[string]string{"HX-Request": "true", "HX-Target": "nope"}, http.StatusNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := serveFragment(fragmentAdapter(&regionRenderer{}), fragmentRequest(tt.headers))

			if rec.Code != tt.code {
				t.Fatalf("code = %d, want %d", rec.Code, tt.code)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
		})
	}
}

// assertVary compares field names rather than searching the value, so a field
// that merely contains another's name cannot stand in for it.
func assertVary(t *testing.T, rec *httptest.ResponseRecorder, want ...string) {
	t.Helper()

	got := map[string]bool{}
	for _, line := range rec.Header().Values("Vary") {
		for _, name := range strings.Split(line, ",") {
			got[strings.ToLower(strings.TrimSpace(name))] = true
		}
	}
	for _, name := range want {
		if !got[strings.ToLower(name)] {
			t.Errorf("Vary = %q, missing %s", rec.Header().Values("Vary"), name)
		}
	}
	if len(got) != len(want) {
		t.Errorf("Vary = %q, want exactly %v", rec.Header().Values("Vary"), want)
	}
}

// A response that does not opt in keeps answering the one way, whatever the
// request carries.
func TestWithoutFragmentTheHeadersAreIgnored(t *testing.T) {
	r := &regionRenderer{}
	rec := httptest.NewRecorder()
	fragmentAdapter(r).Wrap(func(*web.Request) (web.Response, error) {
		return web.Template("dash/page", nil), nil
	})(rec, fragmentRequest(map[string]string{"HX-Request": "true", "HX-Target": "results"}))

	if r.block != "layout" {
		t.Errorf("rendered %q, want the adapter's block", r.block)
	}
	if got := rec.Header().Get("Vary"); got != "" {
		t.Errorf("Vary = %q, want none: this URL has one representation", got)
	}
}

// Block still says what to render rather than asking, which is what serves a
// swap whose target has no name the page knows.
func TestBlockIgnoresTheTargetWithoutFragment(t *testing.T) {
	r := &regionRenderer{}
	rec := httptest.NewRecorder()
	fragmentAdapter(r).Wrap(func(*web.Request) (web.Response, error) {
		return web.Template("todo/list", nil).Block("row"), nil
	})(rec, fragmentRequest(map[string]string{"HX-Request": "true", "HX-Target": "todo-123"}))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	if r.block != "row" {
		t.Errorf("rendered %q, want row", r.block)
	}
}

// Set together, Block is what everything other than a swap is answered with.
// Only an htmx swap is answered by target.
func TestFragmentFallsBackToBlock(t *testing.T) {
	for _, tt := range []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"ordinary request", nil, "content"},
		{"boosted navigation", map[string]string{"HX-Request": "true", "HX-Boosted": "true"}, "content"},
		{"htmx swap", map[string]string{"HX-Request": "true", "HX-Target": "results"}, "region/results"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := &regionRenderer{}
			rec := httptest.NewRecorder()
			fragmentAdapter(r).Wrap(func(*web.Request) (web.Response, error) {
				return web.Template("dash/page", nil).Block("content").Fragment(), nil
			})(rec, fragmentRequest(tt.headers))

			if rec.Code != http.StatusOK {
				t.Fatalf("code = %d", rec.Code)
			}
			if r.block != tt.want {
				t.Errorf("rendered %q, want %q", r.block, tt.want)
			}
		})
	}
}

// Only what htmx sends selects a region. A value that merely looks affirmative
// is a client this was not built for.
func TestFragmentNeedsExactlyTrue(t *testing.T) {
	for _, value := range []string{"True", "TRUE", "1", "yes", ""} {
		t.Run(value, func(t *testing.T) {
			r := &regionRenderer{}
			serveFragment(fragmentAdapter(r), fragmentRequest(map[string]string{
				"HX-Request": value,
				"HX-Target":  "results",
			}))
			if r.block != "layout" {
				t.Errorf("HX-Request %q rendered %q, want the page", value, r.block)
			}
		})
	}
}

// The refusals are reachable by identity, so an application can answer them
// its own way.
func TestFragmentRefusalsAreIdentifiable(t *testing.T) {
	for _, tt := range []struct {
		headers map[string]string
		want    error
	}{
		{map[string]string{"HX-Request": "true"}, web.ErrNoTarget},
		{map[string]string{"HX-Request": "true", "HX-Target": "nope"}, web.ErrUnknownRegion},
	} {
		var got error
		a := web.NewAdapter(
			web.WithRenderer(&regionRenderer{}),
			web.WithBlock("layout"),
			web.WithErrorHandler(func(w http.ResponseWriter, r *http.Request, err error) { got = err }),
		)
		serveFragment(a, fragmentRequest(tt.headers))

		if !errors.Is(got, tt.want) {
			t.Errorf("error = %v, want one matching %v", got, tt.want)
		}
	}
}

func TestFragmentNeedsARegionRenderer(t *testing.T) {
	rec := serveFragment(fragmentAdapter(plainRenderer{}), fragmentRequest(nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d, want 500: this is a misconfiguration", rec.Code)
	}
}

// Chaining returns copies, so opting one response in does not opt in another.
func TestFragmentChainingDoesNotMutate(t *testing.T) {
	base := web.Template("dash/page", nil)
	frag := base.Fragment()

	r := &regionRenderer{}
	rec := httptest.NewRecorder()
	fragmentAdapter(r).Wrap(func(*web.Request) (web.Response, error) {
		return base, nil
	})(rec, fragmentRequest(map[string]string{"HX-Request": "true", "HX-Target": "results"}))

	if r.block != "layout" {
		t.Errorf("the base rendered %q, want layout", r.block)
	}

	rec2 := httptest.NewRecorder()
	fragmentAdapter(r).Wrap(func(*web.Request) (web.Response, error) {
		return frag, nil
	})(rec2, fragmentRequest(map[string]string{"HX-Request": "true", "HX-Target": "results"}))

	if r.block != "region/results" {
		t.Errorf("the copy rendered %q, want region/results", r.block)
	}
}

// Status and headers survive the fragment path.
func TestFragmentKeepsStatusAndHeaders(t *testing.T) {
	r := &regionRenderer{}
	rec := httptest.NewRecorder()
	fragmentAdapter(r).Wrap(func(*web.Request) (web.Response, error) {
		return web.Template("dash/page", nil).
			Fragment().
			Status(http.StatusAccepted).
			Header("X-Kind", "region"), nil
	})(rec, fragmentRequest(map[string]string{"HX-Request": "true", "HX-Target": "results"}))

	if rec.Code != http.StatusAccepted {
		t.Errorf("code = %d, want 202", rec.Code)
	}
	if got := rec.Header().Get("X-Kind"); got != "region" {
		t.Errorf("X-Kind = %q", got)
	}
}
