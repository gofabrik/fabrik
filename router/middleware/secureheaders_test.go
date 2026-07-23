package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var baseline = map[string]string{
	"Content-Security-Policy":           "default-src 'self'; form-action 'self'; base-uri 'self'; object-src 'none'; frame-ancestors 'none'",
	"X-Content-Type-Options":            "nosniff",
	"X-Frame-Options":                   "DENY",
	"Referrer-Policy":                   "no-referrer",
	"Permissions-Policy":                "geolocation=(), camera=(), microphone=(), payment=(), usb=()",
	"Cross-Origin-Opener-Policy":        "same-origin",
	"Cross-Origin-Resource-Policy":      "same-origin",
	"X-Permitted-Cross-Domain-Policies": "none",
	"X-DNS-Prefetch-Control":            "off",
}

func serve(mw func(http.Handler) http.Handler, tls bool, handler http.Handler) http.Header {
	if handler == nil {
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if tls {
		req = httptest.NewRequest(http.MethodGet, "https://example.test/", nil)
	}
	mw(handler).ServeHTTP(rec, req)
	return rec.Result().Header
}

func TestSecureHeadersBaselineWithoutHSTS(t *testing.T) {
	h := serve(SecureHeaders(), false, nil)
	for name, want := range baseline {
		if got := h.Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	if h.Get("Strict-Transport-Security") != "" {
		t.Fatal("HSTS sent without WithHSTS")
	}
	if h.Get("X-XSS-Protection") != "" {
		t.Fatal("X-XSS-Protection must never be emitted")
	}
	// No unlisted headers, including COEP, are emitted.
	if len(h) != len(baseline) {
		t.Fatalf("header set has %d entries, want %d: %v", len(h), len(baseline), h)
	}
}

func TestSecureHeadersHSTSOptIn(t *testing.T) {
	if h := serve(SecureHeaders(WithHSTS()), false, nil); h.Get("Strict-Transport-Security") != "" {
		t.Fatal("HSTS on plain HTTP")
	}
	h := serve(SecureHeaders(WithHSTS()), true, nil)
	if got := h.Get("Strict-Transport-Security"); got != "max-age=63072000" {
		t.Fatalf("HSTS = %q", got)
	}
	h = serve(SecureHeaders(WithHSTSIncludeSubDomains()), true, nil)
	if got := h.Get("Strict-Transport-Security"); got != "max-age=63072000; includeSubDomains" {
		t.Fatalf("HSTS with subdomains = %q", got)
	}
	h = serve(SecureHeaders(WithForceHSTS()), false, nil)
	if h.Get("Strict-Transport-Security") == "" {
		t.Fatal("WithForceHSTS must emit without TLS")
	}
}

func TestSecureHeadersHSTSMaxAge(t *testing.T) {
	h := serve(SecureHeaders(WithHSTSMaxAge(24*time.Hour)), true, nil)
	if got := h.Get("Strict-Transport-Security"); got != "max-age=86400" {
		t.Fatalf("custom max-age = %q", got)
	}
	// Fractional durations truncate to whole seconds.
	h = serve(SecureHeaders(WithHSTSMaxAge(1500*time.Millisecond)), true, nil)
	if got := h.Get("Strict-Transport-Security"); got != "max-age=1" {
		t.Fatalf("fractional max-age = %q", got)
	}
	h = serve(SecureHeaders(WithHSTSMaxAge(0)), true, nil)
	if got := h.Get("Strict-Transport-Security"); got != "max-age=0" {
		t.Fatalf("withdrawal = %q", got)
	}
	// Lifetime and subdomain options compose in either order.
	for _, mw := range []func(http.Handler) http.Handler{
		SecureHeaders(WithHSTSMaxAge(24*time.Hour), WithHSTSIncludeSubDomains()),
		SecureHeaders(WithHSTSIncludeSubDomains(), WithHSTSMaxAge(24*time.Hour)),
	} {
		if got := serve(mw, true, nil).Get("Strict-Transport-Security"); got != "max-age=86400; includeSubDomains" {
			t.Fatalf("composed HSTS = %q", got)
		}
	}
	defer func() {
		if recover() == nil {
			t.Fatal("negative max-age must panic")
		}
	}()
	SecureHeaders(WithHSTSMaxAge(-time.Second))
}

func TestSecureHeadersTypedOverrides(t *testing.T) {
	cases := []struct {
		opt    SecureHeadersOption
		header string
		want   string
	}{
		{WithContentSecurityPolicy("default-src 'none'"), "Content-Security-Policy", "default-src 'none'"},
		{WithReferrerPolicy("strict-origin-when-cross-origin"), "Referrer-Policy", "strict-origin-when-cross-origin"},
		{WithPermissionsPolicy("camera=()"), "Permissions-Policy", "camera=()"},
		{WithCrossOriginOpenerPolicy("same-origin-allow-popups"), "Cross-Origin-Opener-Policy", "same-origin-allow-popups"},
		{WithCrossOriginResourcePolicy("cross-origin"), "Cross-Origin-Resource-Policy", "cross-origin"},
		{WithXFrameOptions("SAMEORIGIN"), "X-Frame-Options", "SAMEORIGIN"},
	}
	for _, c := range cases {
		if got := serve(SecureHeaders(c.opt), false, nil).Get(c.header); got != c.want {
			t.Fatalf("%s = %q, want %q", c.header, got, c.want)
		}
	}
}

func TestSecureHeadersWithoutHeader(t *testing.T) {
	h := serve(SecureHeaders(WithoutHeader("X-DNS-Prefetch-Control")), false, nil)
	if _, present := h["X-Dns-Prefetch-Control"]; present {
		t.Fatal("omitted header sent (canonicalization)")
	}
	// Omission dominates HSTS enablement in either option order.
	for _, mw := range []func(http.Handler) http.Handler{
		SecureHeaders(WithoutHeader("Strict-Transport-Security"), WithForceHSTS()),
		SecureHeaders(WithForceHSTS(), WithoutHeader("Strict-Transport-Security")),
		SecureHeaders(WithoutHeader("Strict-Transport-Security"), WithHSTS()),
		SecureHeaders(WithHSTS(), WithoutHeader("Strict-Transport-Security")),
	} {
		if serve(mw, true, nil).Get("Strict-Transport-Security") != "" {
			t.Fatal("omission must dominate HSTS options")
		}
	}
	defer func() {
		if recover() == nil {
			t.Fatal("unsupported header name must panic")
		}
	}()
	SecureHeaders(WithoutHeader("X-Made-Up"))
}

func TestSecureHeadersValueValidation(t *testing.T) {
	for _, bad := range []string{"", "   ", "\t", "a\r\nb", "a\x00b", "a\x7fb"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("value %q must panic at construction", bad)
				}
			}()
			SecureHeaders(WithReferrerPolicy(bad))
		}()
	}
	// Printable ASCII and tabs are accepted.
	ok := "no-referrer, strict-origin \tx" + strings.Repeat("~", 3)
	serve(SecureHeaders(WithReferrerPolicy(ok)), false, nil)
}

func TestSecureHeadersHandlerOverrideWins(t *testing.T) {
	h := serve(SecureHeaders(), false, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cross-Origin-Resource-Policy", "cross-origin")
		w.WriteHeader(http.StatusOK)
	}))
	if got := h.Get("Cross-Origin-Resource-Policy"); got != "cross-origin" {
		t.Fatalf("handler override lost: %q", got)
	}
}

func TestSecureHeadersPresentOnBareWriteHeader(t *testing.T) {
	h := serve(SecureHeaders(), false, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	if h.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("headers missing on a WriteHeader-only response")
	}
}
