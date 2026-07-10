package web_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/web"
)

type loginPage struct{ Error string }

func (loginPage) Template() string { return "auth/login" }

func login(req *web.Request) (web.Response, error) {
	if req.FormValue("email") == "" {
		return web.View(loginPage{Error: "missing email"}), nil
	}
	req.SetCookie("session", "tok", web.CookieSecure(), web.CookieHTTPOnly())
	return web.Redirect("/account"), nil
}

func TestHandlerAsFunction(t *testing.T) {
	req := httptest.NewRequest("POST", "/login?email=a@b", nil)
	resp, err := login(webRequest(t, req))
	if err != nil {
		t.Fatal(err)
	}
	if resp != web.Redirect("/account") { // comparable responses compare directly
		t.Fatalf("resp = %#v, want redirect", resp)
	}
}

func webRequest(t *testing.T, r *http.Request) *web.Request {
	t.Helper()
	var captured *web.Request
	web.NewAdapter().Wrap(func(req *web.Request) (web.Response, error) {
		captured = req
		return web.Status(http.StatusNoContent), nil
	})(httptest.NewRecorder(), r)
	if captured == nil {
		t.Fatal("no request captured")
	}
	return captured
}

func TestWrapAppliesRecordedStateOnSuccess(t *testing.T) {
	a := web.NewAdapter()
	h := a.Wrap(func(req *web.Request) (web.Response, error) {
		req.SetHeader("X-Trace", "recorded")
		req.SetCookie("session", "tok")
		return web.Status(http.StatusNoContent), nil
	})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("code = %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("Status must write no body byte, got %q", rec.Body.String())
	}
	if rec.Header().Get("X-Trace") != "recorded" {
		t.Fatal("recorded header not applied")
	}
	if !strings.Contains(rec.Header().Get("Set-Cookie"), "session=tok") {
		t.Fatalf("cookie not applied: %q", rec.Header().Get("Set-Cookie"))
	}
}

func TestWrapDropsRecordedStateOnError(t *testing.T) {
	var handled error
	a := web.NewAdapter(web.WithErrorHandler(func(w http.ResponseWriter, r *http.Request, err error) {
		handled = err
		w.WriteHeader(http.StatusInternalServerError)
	}))
	h := a.Wrap(func(req *web.Request) (web.Response, error) {
		req.SetCookie("session", "must-not-leak")
		return nil, errors.New("boom")
	})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/", nil))
	if handled == nil || handled.Error() != "boom" {
		t.Fatalf("error handler got %v", handled)
	}
	if rec.Header().Get("Set-Cookie") != "" {
		t.Fatalf("failed handler leaked a cookie: %q", rec.Header().Get("Set-Cookie"))
	}
}

func TestWrapNilNilIsErrNilResponse(t *testing.T) {
	var handled error
	a := web.NewAdapter(web.WithErrorHandler(func(w http.ResponseWriter, r *http.Request, err error) {
		handled = err
	}))
	a.Wrap(func(*web.Request) (web.Response, error) { return nil, nil })(
		httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if !errors.Is(handled, web.ErrNilResponse) {
		t.Fatalf("got %v, want ErrNilResponse", handled)
	}
}

func TestResponseHeaderWinsOverRecorded(t *testing.T) {
	a := web.NewAdapter()
	h := a.Wrap(func(req *web.Request) (web.Response, error) {
		req.SetHeader("Content-Type", "recorded/should-lose")
		return web.Text(http.StatusOK, "hi"), nil
	})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/", nil))
	if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want the response's to win", got)
	}
}

type fakeRenderer struct{ name string }

func (f *fakeRenderer) Render(w http.ResponseWriter, name string, data any) error {
	f.name = name
	_, err := w.Write([]byte("rendered"))
	return err
}

func TestViewRendersThroughRenderer(t *testing.T) {
	r := &fakeRenderer{}
	a := web.NewAdapter(web.WithRenderer(r))
	rec := httptest.NewRecorder()
	a.Wrap(func(*web.Request) (web.Response, error) {
		return web.View(loginPage{}), nil
	})(rec, httptest.NewRequest("GET", "/", nil))
	if r.name != "auth/login" || rec.Body.String() != "rendered" {
		t.Fatalf("render = %q body %q", r.name, rec.Body.String())
	}
}

func TestViewWithoutRendererFailsThroughErrorPath(t *testing.T) {
	var handled error
	a := web.NewAdapter(web.WithErrorHandler(func(w http.ResponseWriter, r *http.Request, err error) {
		handled = err
	}))
	a.Wrap(func(*web.Request) (web.Response, error) {
		return web.View(loginPage{}), nil
	})(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if handled == nil || !strings.Contains(handled.Error(), "without a renderer") {
		t.Fatalf("got %v", handled)
	}
}

func TestJSONBufferedEncodeErrorReachesErrorHandler(t *testing.T) {
	var handled error
	a := web.NewAdapter(web.WithErrorHandler(func(w http.ResponseWriter, r *http.Request, err error) {
		handled = err
	}))
	rec := httptest.NewRecorder()
	a.Wrap(func(*web.Request) (web.Response, error) {
		return web.JSON{Value: make(chan int)}, nil // unencodable
	})(rec, httptest.NewRequest("GET", "/", nil))
	if handled == nil {
		t.Fatal("encode error not routed")
	}
	if rec.Body.Len() != 0 && rec.Header().Get("Content-Type") == "application/json; charset=utf-8" {
		t.Fatal("partial JSON written despite encode error")
	}
}

func TestJSONResponds(t *testing.T) {
	rec := httptest.NewRecorder()
	web.NewAdapter().Wrap(func(*web.Request) (web.Response, error) {
		return web.JSON{Value: map[string]int{"n": 1}}, nil
	})(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 || rec.Body.String() != `{"n":1}` {
		t.Fatalf("json = %d %q", rec.Code, rec.Body.String())
	}
}

func TestRedirectSeeOther(t *testing.T) {
	rec := httptest.NewRecorder()
	web.NewAdapter().Wrap(func(*web.Request) (web.Response, error) {
		return web.Redirect("/next"), nil
	})(rec, httptest.NewRequest("POST", "/", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/next" {
		t.Fatalf("redirect = %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestCookieAccessors(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: "session", Value: "tok"})
	req := webRequest(t, r)
	if v, ok := req.Cookie("session"); !ok || v != "tok" {
		t.Fatalf("Cookie = %q %v", v, ok)
	}
	if _, ok := req.Cookie("missing"); ok {
		t.Fatal("missing cookie reported present")
	}
}

func TestClearCookieExpires(t *testing.T) {
	a := web.NewAdapter()
	rec := httptest.NewRecorder()
	a.Wrap(func(req *web.Request) (web.Response, error) {
		req.ClearCookie("session")
		return web.Status(http.StatusNoContent), nil
	})(rec, httptest.NewRequest("GET", "/", nil))
	if sc := rec.Header().Get("Set-Cookie"); !strings.Contains(sc, "Max-Age=0") {
		t.Fatalf("ClearCookie = %q, want an expiring cookie", sc)
	}
}

func TestPostCommitErrorIsNotReRendered(t *testing.T) {
	var handled error
	a := web.NewAdapter(web.WithErrorHandler(func(w http.ResponseWriter, r *http.Request, err error) {
		handled = err
	}))
	rec := httptest.NewRecorder()
	a.Wrap(func(*web.Request) (web.Response, error) {
		return failAfterCommit{}, nil
	})(rec, httptest.NewRequest("GET", "/", nil))
	if handled != nil {
		t.Fatalf("post-commit error re-rendered through error handler: %v", handled)
	}
	if rec.Body.String() != "partial" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

type failAfterCommit struct{}

func (failAfterCommit) Respond(w http.ResponseWriter, r *http.Request) error {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("partial"))
	return errors.New("stream broke")
}

// View responses carry no adapter state.
var sharedHome = web.View(loginPage{Error: "none"})

func TestViewValuesAreShareable(t *testing.T) {
	r1 := &fakeRenderer{}
	a1 := web.NewAdapter(web.WithRenderer(r1))
	rec := httptest.NewRecorder()
	a1.Wrap(func(*web.Request) (web.Response, error) { return sharedHome, nil })(
		rec, httptest.NewRequest("GET", "/", nil))
	if r1.name != "auth/login" {
		t.Fatalf("first adapter rendered %q", r1.name)
	}

	var handled error
	a2 := web.NewAdapter(web.WithErrorHandler(func(w http.ResponseWriter, r *http.Request, err error) {
		handled = err
	}))
	a2.Wrap(func(*web.Request) (web.Response, error) { return sharedHome, nil })(
		httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if handled == nil || !strings.Contains(handled.Error(), "without a renderer") {
		t.Fatalf("second adapter = %v, want renderer error - shared view leaked state", handled)
	}
}

func TestTemplateResponse(t *testing.T) {
	r := &fakeRenderer{}
	a := web.NewAdapter(web.WithRenderer(r))
	rec := httptest.NewRecorder()
	a.Wrap(func(*web.Request) (web.Response, error) {
		return web.Template("emails/welcome", map[string]string{"Name": "x"}), nil
	})(rec, httptest.NewRequest("GET", "/", nil))
	if r.name != "emails/welcome" || rec.Body.String() != "rendered" {
		t.Fatalf("Template = %q %q", r.name, rec.Body.String())
	}
}

func TestCommitWriterForwardsFlush(t *testing.T) {
	a := web.NewAdapter()
	rec := httptest.NewRecorder()
	a.Wrap(func(*web.Request) (web.Response, error) {
		return flusher{}, nil
	})(rec, httptest.NewRequest("GET", "/", nil))
	if !rec.Flushed {
		t.Fatal("Flush not forwarded through the adapter's writer")
	}
}

type flusher struct{}

func (flusher) Respond(w http.ResponseWriter, r *http.Request) error {
	w.Write([]byte("chunk"))
	if f, ok := w.(http.Flusher); !ok {
		return errors.New("writer lost http.Flusher")
	} else {
		f.Flush()
	}
	return nil
}

// plainWriter lacks http.Flusher.
type plainWriter struct {
	h    http.Header
	code int
	body []byte
}

func (p *plainWriter) Header() http.Header { return p.h }
func (p *plainWriter) WriteHeader(c int)   { p.code = c }
func (p *plainWriter) Write(b []byte) (int, error) {
	p.body = append(p.body, b...)
	return len(b), nil
}

func TestFlusherOnlyWhenUnderlyingSupportsIt(t *testing.T) {
	var sawFlusher bool
	a := web.NewAdapter()
	// Expose Flusher when the underlying writer supports it.
	rec := httptest.NewRecorder()
	a.Wrap(func(*web.Request) (web.Response, error) {
		return probeResponse{&sawFlusher}, nil
	})(rec, httptest.NewRequest("GET", "/", nil))
	if !sawFlusher {
		t.Fatal("Flusher hidden despite underlying support")
	}

	// Do not add Flusher when the underlying writer lacks it.
	a.Wrap(func(*web.Request) (web.Response, error) {
		return probeResponse{&sawFlusher}, nil
	})(&plainWriter{h: http.Header{}}, httptest.NewRequest("GET", "/", nil))
	if sawFlusher {
		t.Fatal("wrapper claims Flusher over a non-flushing writer")
	}
}

type probeResponse struct{ saw *bool }

func (p probeResponse) Respond(w http.ResponseWriter, r *http.Request) error {
	_, *p.saw = w.(http.Flusher)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func TestRespondFailureCarriesNoRecordedState(t *testing.T) {
	a := web.NewAdapter(web.WithErrorHandler(func(w http.ResponseWriter, r *http.Request, err error) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	rec := httptest.NewRecorder()
	a.Wrap(func(req *web.Request) (web.Response, error) {
		req.SetHeader("X-Trace", "recorded")
		req.SetCookie("session", "must-not-leak")
		return web.View(loginPage{}), nil // fails: no renderer configured
	})(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d", rec.Code)
	}
	if rec.Header().Get("Set-Cookie") != "" || rec.Header().Get("X-Trace") != "" {
		t.Fatalf("error response carries recorded state: cookie=%q header=%q",
			rec.Header().Get("Set-Cookie"), rec.Header().Get("X-Trace"))
	}
}

func TestStripRestoresMiddlewareOwnedState(t *testing.T) {
	a := web.NewAdapter(web.WithErrorHandler(func(w http.ResponseWriter, r *http.Request, err error) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	inner := a.Wrap(func(req *web.Request) (web.Response, error) {
		req.SetHeader("X-Kind", "handler")
		req.SetCookie("session", "must-not-leak")
		return web.View(loginPage{}), nil // fails pre-commit: no renderer
	})
	// Middleware outside Wrap owns response state of its own.
	outer := func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "refresh", Value: "kept"})
		w.Header().Set("X-Kind", "middleware")
		inner(w, r)
	}
	rec := httptest.NewRecorder()
	outer(rec, httptest.NewRequest("GET", "/", nil))

	cookies := rec.Header().Values("Set-Cookie")
	if len(cookies) != 1 || !strings.Contains(cookies[0], "refresh=kept") {
		t.Fatalf("Set-Cookie = %v, want only the middleware's cookie", cookies)
	}
	if got := rec.Header().Get("X-Kind"); got != "middleware" {
		t.Fatalf("X-Kind = %q, want the middleware's value restored", got)
	}
}

func TestSetHeaderCanonicalizes(t *testing.T) {
	a := web.NewAdapter(web.WithErrorHandler(func(w http.ResponseWriter, r *http.Request, err error) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	// Case variants are one header; the strip removes it entirely.
	rec := httptest.NewRecorder()
	a.Wrap(func(req *web.Request) (web.Response, error) {
		req.SetHeader("x-trace", "lower")
		req.SetHeader("X-Trace", "canonical") // same header, later wins
		return web.View(loginPage{}), nil     // fails pre-commit
	})(rec, httptest.NewRequest("GET", "/", nil))
	for key := range rec.Header() {
		if strings.EqualFold(key, "X-Trace") {
			t.Fatalf("recorded header survived the strip under key %q", key)
		}
	}

	rec = httptest.NewRecorder()
	a.Wrap(func(req *web.Request) (web.Response, error) {
		req.SetHeader("x-trace", "lower")
		req.SetHeader("X-Trace", "canonical")
		return web.Status(http.StatusNoContent), nil
	})(rec, httptest.NewRequest("GET", "/", nil))
	if got := rec.Header().Values("X-Trace"); len(got) != 1 || got[0] != "canonical" {
		t.Fatalf("X-Trace = %v, want the single later value", got)
	}
}

func TestJSONStatusOverride(t *testing.T) {
	rec := httptest.NewRecorder()
	web.NewAdapter().Wrap(func(*web.Request) (web.Response, error) {
		return web.JSON{Status: http.StatusCreated, Value: map[string]int{"id": 7}}, nil
	})(rec, httptest.NewRequest("POST", "/", nil))
	if rec.Code != http.StatusCreated || rec.Body.String() != `{"id":7}` {
		t.Fatalf("json = %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("content type = %q", rec.Header().Get("Content-Type"))
	}
}

func TestCookiePathAndSameSite(t *testing.T) {
	rec := httptest.NewRecorder()
	web.NewAdapter().Wrap(func(req *web.Request) (web.Response, error) {
		req.SetCookie("session", "tok",
			web.CookiePath("/app"), web.CookieSameSite(http.SameSiteLaxMode), web.CookieSecure())
		return web.Status(http.StatusNoContent), nil
	})(rec, httptest.NewRequest("GET", "/", nil))
	sc := rec.Header().Get("Set-Cookie")
	for _, want := range []string{"Path=/app", "SameSite=Lax", "Secure"} {
		if !strings.Contains(sc, want) {
			t.Fatalf("Set-Cookie = %q, missing %s", sc, want)
		}
	}
}

func TestPreCommitRollbackCoversResponseSetHeaders(t *testing.T) {
	a := web.NewAdapter(web.WithErrorHandler(func(w http.ResponseWriter, r *http.Request, err error) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	rec := httptest.NewRecorder()
	rec.Header().Set("X-Owner", "middleware")
	a.Wrap(func(*web.Request) (web.Response, error) {
		return headerThenFail{}, nil
	})(rec, httptest.NewRequest("GET", "/", nil))
	if got := rec.Header().Get("X-RateLimit"); got != "" {
		t.Fatalf("response-set header leaked onto the error response: %q", got)
	}
	if got := rec.Header().Get("X-Owner"); got != "middleware" {
		t.Fatalf("middleware-owned header lost: %q", got)
	}
}

type headerThenFail struct{}

func (headerThenFail) Respond(w http.ResponseWriter, r *http.Request) error {
	w.Header().Set("X-RateLimit", "10") // touched, then fails pre-commit
	return errors.New("gave up before committing")
}
