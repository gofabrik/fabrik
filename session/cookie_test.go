package session

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestCookie_WriteAndRead(t *testing.T) {
	c := Cookie{Name: "sid", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}
	w := httptest.NewRecorder()
	exp := time.Now().Add(time.Hour)
	c.Write(w, "abc123", TokenWriteOptions{Expiry: exp})

	r := &http.Request{Header: http.Header{"Cookie": w.Header()["Set-Cookie"]}}
	got, ok := c.Read(r)
	if !ok || got != "abc123" {
		t.Fatalf("read: ok=%v got=%q", ok, got)
	}

	parsed := w.Result().Cookies()[0]
	if !parsed.Secure || !parsed.HttpOnly || parsed.SameSite != http.SameSiteLaxMode {
		t.Errorf("flags lost: %+v", parsed)
	}
	if parsed.MaxAge <= 0 {
		t.Errorf("MaxAge should be set from Expiry, got %d", parsed.MaxAge)
	}
}

func TestCookie_ReadMissing(t *testing.T) {
	c := Cookie{}
	r := httptest.NewRequest("GET", "/", nil)
	if _, ok := c.Read(r); ok {
		t.Errorf("expected ok=false on missing cookie")
	}
}

func TestCookie_Clear(t *testing.T) {
	c := Cookie{Name: "sid"}
	w := httptest.NewRecorder()
	c.Clear(w)
	got := w.Result().Cookies()[0]
	if got.MaxAge >= 0 {
		t.Errorf("Clear should set negative MaxAge, got %d", got.MaxAge)
	}
	if got.Value != "" {
		t.Errorf("Clear should blank value, got %q", got.Value)
	}
}

func TestCookie_DefaultsName(t *testing.T) {
	c := Cookie{}
	w := httptest.NewRecorder()
	c.Write(w, "v", TokenWriteOptions{})
	if w.Result().Cookies()[0].Name != defaultCookieName {
		t.Errorf("missing default cookie name")
	}
}

func TestCookie_CustomPathAndDomainSurviveSetCookie(t *testing.T) {
	c := Cookie{Name: "sid", Path: "/api", Domain: "example.com"}
	w := httptest.NewRecorder()
	c.Write(w, "abc", TokenWriteOptions{})
	got := w.Result().Cookies()[0]
	if got.Path != "/api" {
		t.Errorf("Path: want /api, got %q", got.Path)
	}
	if got.Domain != "example.com" {
		t.Errorf("Domain: want example.com, got %q", got.Domain)
	}
}

func TestCookie_PathScopingViaCookieJar(t *testing.T) {
	// End-to-end through net/http/cookiejar: a cookie set with
	// Path=/api must be sent on /api/x but withheld from /other.
	mgr, err := New[cart](Config{
		Store:          NewMemoryStore(),
		Token:          Cookie{Name: "sid", Path: "/api"},
		AbsoluteExpiry: time.Hour,
		IdleExpiry:     time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := mgr.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Always touch the session so the cookie ships.
		if err := mgr.Update(r.Context(), func(c *cart) error { return nil }); err != nil {
			t.Errorf("update session: %v", err)
		}
	}))
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		Jar: jar,
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, r)
			return rec.Result(), nil
		}),
	}
	baseURL := &url.URL{Scheme: "http", Host: "session.test"}

	// First request to /api/login establishes the cookie at Path=/api.
	resp, err := client.Get(baseURL.ResolveReference(&url.URL{Path: "/api/login"}).String())
	if err != nil {
		t.Fatal(err)
	}
	// #nosec G104 -- response body Close is read cleanup
	resp.Body.Close() //nolint:errcheck // response body Close is read cleanup

	scoped := jar.Cookies(&url.URL{Scheme: baseURL.Scheme, Host: baseURL.Host, Path: "/api/anything"})
	if len(scoped) != 1 || scoped[0].Name != "sid" {
		t.Errorf("jar should hold the sid cookie under /api, got %v", scoped)
	}
	outside := jar.Cookies(&url.URL{Scheme: baseURL.Scheme, Host: baseURL.Host, Path: "/other"})
	if len(outside) != 0 {
		t.Errorf("Path=/api cookie leaked to /other: %v", outside)
	}
}

// cart is the transport tests' cell payload.
type cart struct {
	Items []string
}

// An injected clock keeps the cookie's MaxAge coherent with the
// session's expiry instead of measuring from the wall clock.
func TestCookieMaxAgeUsesInjectedClock(t *testing.T) {
	base := time.Now().Add(-1000 * time.Hour) // far from wall time
	w := httptest.NewRecorder()
	Cookie{Name: "sid"}.Write(w, "abc", TokenWriteOptions{
		Expiry: base.Add(10 * time.Minute),
		Now:    base,
	})
	got := w.Result().Cookies()[0]
	if got.MaxAge < 590 || got.MaxAge > 600 {
		t.Fatalf("MaxAge = %d, want ~600 measured from the injected clock", got.MaxAge)
	}
}
