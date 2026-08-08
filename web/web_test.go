package web_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/web"
)

type loginPage struct{ Error string }

func login(req *web.Request) (web.Response, error) {
	if req.FormValue("email") == "" {
		return web.Template("auth/login", loginPage{Error: "missing email"}), nil
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

func (f *fakeRenderer) Render(w io.Writer, name, block string, data any) error {
	f.name = name
	_, err := w.Write([]byte("rendered"))
	return err
}

func TestTemplateRendersThroughRenderer(t *testing.T) {
	r := &fakeRenderer{}
	a := web.NewAdapter(web.WithRenderer(r))
	rec := httptest.NewRecorder()
	a.Wrap(func(*web.Request) (web.Response, error) {
		return web.Template("auth/login", loginPage{}), nil
	})(rec, httptest.NewRequest("GET", "/", nil))
	if r.name != "auth/login" || rec.Body.String() != "rendered" {
		t.Fatalf("render = %q body %q", r.name, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want the adapter to label rendered responses", ct)
	}
}

var errRenderExploded = errors.New("render exploded")

type failingRenderer struct{}

func (failingRenderer) Render(io.Writer, string, string, any) error {
	return errRenderExploded
}

func TestTemplateRenderFailureRestoresHeadersAndErrors(t *testing.T) {
	var handled error
	a := web.NewAdapter(web.WithRenderer(failingRenderer{}),
		web.WithErrorHandler(func(w http.ResponseWriter, r *http.Request, err error) {
			handled = err
			w.WriteHeader(http.StatusInternalServerError)
		}))
	rec := httptest.NewRecorder()
	a.Wrap(func(*web.Request) (web.Response, error) {
		return web.Template("auth/login", loginPage{}), nil
	})(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 through the error handler", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want the pre-render header restored", ct)
	}
	if want := "web: render auth/login: render exploded"; handled == nil || handled.Error() != want {
		t.Fatalf("handled error = %v, want %q", handled, want)
	}
	if !errors.Is(handled, errRenderExploded) {
		t.Fatalf("Template render error not wrapped with %%w: %v", handled)
	}
}

func TestTemplateWithoutRendererFailsThroughErrorPath(t *testing.T) {
	var handled error
	a := web.NewAdapter(web.WithErrorHandler(func(w http.ResponseWriter, r *http.Request, err error) {
		handled = err
	}))
	a.Wrap(func(*web.Request) (web.Response, error) {
		return web.Template("auth/login", loginPage{}), nil
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
		return web.JSON(make(chan int)), nil // unencodable
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
		return web.JSON(map[string]int{"n": 1}), nil
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
	// #nosec G124 -- cookie attributes are caller-configurable
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
	if _, err := w.Write([]byte("partial")); err != nil {
		return err
	}
	return errors.New("stream broke")
}

// Template responses carry no adapter state.
var sharedHome = web.Template("auth/login", loginPage{Error: "none"})

func TestTemplateValuesAreShareable(t *testing.T) {
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
		t.Fatalf("second adapter = %v, want renderer error - shared template response leaked state", handled)
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
	if _, err := w.Write([]byte("chunk")); err != nil {
		return err
	}
	f, ok := w.(http.Flusher)
	if !ok {
		return errors.New("writer lost http.Flusher")
	}
	f.Flush()
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
		return web.Template("auth/login", loginPage{}), nil // fails: no renderer configured
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
		return web.Template("auth/login", loginPage{}), nil // fails pre-commit: no renderer
	})
	// Middleware outside Wrap owns response state of its own.
	outer := func(w http.ResponseWriter, r *http.Request) {
		// #nosec G124 -- cookie attributes are caller-configurable
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
		req.SetHeader("X-Trace", "canonical")               // same header, later wins
		return web.Template("auth/login", loginPage{}), nil // fails pre-commit
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
		return web.JSON(map[string]int{"id": 7}).Status(http.StatusCreated), nil
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

type statusCarrier struct{ status int }

func (e statusCarrier) Error() string   { return "carrier" }
func (e statusCarrier) HTTPStatus() int { return e.status }

func TestErrorStatusAcceptsOnlyErrorRange(t *testing.T) {
	cases := []struct {
		status int
		ok     bool
	}{{400, true}, {413, true}, {599, true}, {503, true}, {200, false}, {399, false}, {600, false}, {0, false}, {-1, false}}
	for _, c := range cases {
		got, ok := web.ErrorStatus(statusCarrier{c.status})
		if ok != c.ok || (ok && got != c.status) {
			t.Errorf("ErrorStatus(%d) = %d, %v; want ok=%v", c.status, got, ok, c.ok)
		}
	}
	if _, ok := web.ErrorStatus(errors.New("plain")); ok {
		t.Error("plain error reported a status")
	}
	if _, ok := web.ErrorStatus(fmt.Errorf("wrapped: %w", statusCarrier{404})); !ok {
		t.Error("wrapped status-bearing error not discovered")
	}

	counter := &countingCarrier{status: 418}
	if status, ok := web.ErrorStatus(counter); !ok || status != 418 {
		t.Fatalf("counting carrier = %d, %v", status, ok)
	}
	if counter.calls != 1 {
		t.Errorf("HTTPStatus called %d times, want exactly once", counter.calls)
	}
}

type countingCarrier struct {
	status int
	calls  int
}

func (e *countingCarrier) Error() string { return "counting" }
func (e *countingCarrier) HTTPStatus() int {
	e.calls++
	return e.status
}

type captureLogs struct{ records *[]slog.Record }

func (c captureLogs) Enabled(context.Context, slog.Level) bool { return true }
func (c captureLogs) Handle(_ context.Context, r slog.Record) error {
	*c.records = append(*c.records, r)
	return nil
}
func (c captureLogs) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c captureLogs) WithGroup(string) slog.Handler      { return c }

func TestDefaultErrorHandlerHonorsStatusBearingErrors(t *testing.T) {
	var records []slog.Record
	prev := slog.Default()
	slog.SetDefault(slog.New(captureLogs{records: &records}))
	t.Cleanup(func() { slog.SetDefault(prev) })

	cases := []struct {
		status    int
		wantCode  int
		wantLevel slog.Level
		wantBody  string
	}{
		{413, 413, slog.LevelInfo, "Request Entity Too Large\n"},
		{503, 503, slog.LevelError, "Service Unavailable\n"},
		{499, 499, slog.LevelInfo, "request error\n"},
		{599, 599, slog.LevelError, "request error\n"},
		{200, 500, slog.LevelError, "internal server error\n"},
		{700, 500, slog.LevelError, "internal server error\n"},
	}
	for _, c := range cases {
		records = records[:0]
		h := web.NewAdapter().Wrap(func(*web.Request) (web.Response, error) {
			return nil, statusCarrier{c.status}
		})
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code != c.wantCode {
			t.Errorf("status %d: code = %d, want %d", c.status, rec.Code, c.wantCode)
		}
		if body := rec.Body.String(); body != c.wantBody {
			t.Errorf("status %d: body = %q, want %q", c.status, body, c.wantBody)
		}
		if len(records) != 1 {
			t.Errorf("status %d: logged %d records, want 1", c.status, len(records))
		} else if records[0].Level != c.wantLevel {
			t.Errorf("status %d: log level = %v, want %v", c.status, records[0].Level, c.wantLevel)
		}
	}
}

func TestCustomErrorHandlerCanReuseErrorStatus(t *testing.T) {
	h := web.NewAdapter(web.WithErrorHandler(func(w http.ResponseWriter, r *http.Request, err error) {
		if status, ok := web.ErrorStatus(err); ok {
			w.WriteHeader(status)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	})).Wrap(func(*web.Request) (web.Response, error) {
		return nil, statusCarrier{429}
	})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != 429 {
		t.Fatalf("custom handler code = %d, want 429", rec.Code)
	}
}

// Both renderer-misconfiguration messages name the surviving option and
// carry sentinel identities.
func TestTemplateRendererErrorsArePinned(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	const bareMsg = "web: Template responses render through an adapter (configure WithRenderer)"
	if err := web.Template("x", nil).Respond(httptest.NewRecorder(), r); !errors.Is(err, web.ErrTemplateWithoutAdapter) || err.Error() != bareMsg {
		t.Fatalf("bare response error = %v, want %q", err, bareMsg)
	}
	var handled error
	a := web.NewAdapter(web.WithErrorHandler(func(w http.ResponseWriter, r *http.Request, err error) {
		handled = err
	}))
	a.Wrap(func(*web.Request) (web.Response, error) {
		return web.Template("x", nil), nil
	})(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	const adapterMsg = "web: Template response without a renderer (configure WithRenderer)"
	if !errors.Is(handled, web.ErrNoTemplateRenderer) || handled.Error() != adapterMsg {
		t.Fatalf("adapter error = %v, want %q", handled, adapterMsg)
	}
}

// recordingRenderer keeps both names and writes something identifiable.
type recordingRenderer struct {
	name  string
	block string
}

func (r *recordingRenderer) Render(w io.Writer, name, block string, data any) error {
	r.name, r.block = name, block
	_, err := fmt.Fprintf(w, "rendered %s/%s", name, block)
	return err
}

func TestTemplateStatus(t *testing.T) {
	a := web.NewAdapter(web.WithRenderer(&recordingRenderer{}))

	rec := httptest.NewRecorder()
	a.Wrap(func(*web.Request) (web.Response, error) {
		return web.Template("article/form", nil).Status(http.StatusUnprocessableEntity), nil
	})(rec, httptest.NewRequest("GET", "/", nil))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("code = %d, want 422", rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("a status should not cost the body")
	}

	// Unset means 200.
	rec = httptest.NewRecorder()
	a.Wrap(func(*web.Request) (web.Response, error) {
		return web.Template("article/form", nil), nil
	})(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
}

func TestTemplateBlock(t *testing.T) {
	t.Run("adapter default", func(t *testing.T) {
		r := &recordingRenderer{}
		a := web.NewAdapter(web.WithRenderer(r), web.WithBlock("layout"))
		rec := httptest.NewRecorder()
		a.Wrap(func(*web.Request) (web.Response, error) {
			return web.Template("todo/list", nil), nil
		})(rec, httptest.NewRequest("GET", "/", nil))
		if r.block != "layout" {
			t.Fatalf("block = %q, want the adapter default", r.block)
		}
	})

	t.Run("response overrides it", func(t *testing.T) {
		r := &recordingRenderer{}
		a := web.NewAdapter(web.WithRenderer(r), web.WithBlock("layout"))
		rec := httptest.NewRecorder()
		a.Wrap(func(*web.Request) (web.Response, error) {
			return web.Template("todo/list", nil).Block("row"), nil
		})(rec, httptest.NewRequest("GET", "/", nil))
		if r.name != "todo/list" || r.block != "row" {
			t.Fatalf("render = %q/%q, want todo/list/row", r.name, r.block)
		}
	})

	t.Run("no default is an empty block", func(t *testing.T) {
		r := &recordingRenderer{}
		a := web.NewAdapter(web.WithRenderer(r))
		rec := httptest.NewRecorder()
		a.Wrap(func(*web.Request) (web.Response, error) {
			return web.Template("mail/welcome", nil), nil
		})(rec, httptest.NewRequest("GET", "/", nil))
		if r.block != "" {
			t.Fatalf("block = %q, want empty for a renderer with nothing to choose", r.block)
		}
	})
}

// Chaining returns copies, so a response shared between handlers cannot be
// altered by one of them adding a status, a block or a header to it.
func TestTemplateChainingDoesNotMutate(t *testing.T) {
	base := web.Template("article/form", nil)
	derived := base.Status(http.StatusUnprocessableEntity).Block("row").Header("X-Kind", "fragment")

	render := func(resp web.Response) (*httptest.ResponseRecorder, *recordingRenderer) {
		r := &recordingRenderer{}
		rec := httptest.NewRecorder()
		web.NewAdapter(web.WithRenderer(r), web.WithBlock("layout")).
			Wrap(func(*web.Request) (web.Response, error) { return resp, nil })(
			rec, httptest.NewRequest("GET", "/", nil))
		return rec, r
	}

	rec, r := render(base)
	if rec.Code != http.StatusOK || r.block != "layout" || rec.Header().Get("X-Kind") != "" {
		t.Fatalf("the original changed: %d %q %q", rec.Code, r.block, rec.Header().Get("X-Kind"))
	}

	rec, r = render(derived)
	if rec.Code != http.StatusUnprocessableEntity || r.block != "row" || rec.Header().Get("X-Kind") != "fragment" {
		t.Fatalf("the copy did not carry its own: %d %q %q", rec.Code, r.block, rec.Header().Get("X-Kind"))
	}

	// Two chains from the same base must not see each other's headers.
	one := base.Header("X-One", "1")
	two := base.Header("X-Two", "2")
	rec, _ = render(one)
	if rec.Header().Get("X-Two") != "" {
		t.Fatal("one chain leaked a header into another")
	}
	rec, _ = render(two)
	if rec.Header().Get("X-One") != "" {
		t.Fatal("one chain leaked a header into another")
	}
}

func TestJSONHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	web.NewAdapter().Wrap(func(*web.Request) (web.Response, error) {
		return web.JSON(map[string]int{"n": 1}).
			Header("Cache-Control", "no-store").
			Status(http.StatusAccepted), nil
	})(rec, httptest.NewRequest("GET", "/", nil))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want the response's own", got)
	}
}

// A header on the response wins over one recorded on the request.
func TestResponseHeaderBeatsRecorded(t *testing.T) {
	rec := httptest.NewRecorder()
	web.NewAdapter().Wrap(func(req *web.Request) (web.Response, error) {
		req.SetHeader("X-Source", "request")
		return web.HTML(http.StatusOK, "hi").Header("X-Source", "response"), nil
	})(rec, httptest.NewRequest("GET", "/", nil))

	if got := rec.Header().Get("X-Source"); got != "response" {
		t.Fatalf("X-Source = %q, want the response's", got)
	}
}

// A failed render must not leave a status behind: the error handler needs to
// choose one itself.
func TestTemplateStatusNotCommittedWhenRenderFails(t *testing.T) {
	var handled error
	a := web.NewAdapter(
		web.WithRenderer(failingRenderer{}),
		web.WithErrorHandler(func(w http.ResponseWriter, r *http.Request, err error) {
			handled = err
			w.WriteHeader(http.StatusInternalServerError)
		}),
	)

	rec := httptest.NewRecorder()
	a.Wrap(func(*web.Request) (web.Response, error) {
		return web.Template("article/form", nil).Status(http.StatusUnprocessableEntity), nil
	})(rec, httptest.NewRequest("GET", "/", nil))

	if !errors.Is(handled, errRenderExploded) {
		t.Fatalf("error handler saw %v", handled)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want the error handler's 500 rather than the response's 422", rec.Code)
	}
	if rec.Body.String() != "" {
		t.Fatalf("body = %q, want nothing written", rec.Body.String())
	}
}
