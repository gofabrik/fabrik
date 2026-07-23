package middleware

import (
	"fmt"
	"net/http"
	"strconv"
	"time"
)

var secureDefaults = [][2]string{
	// Restrict content to the page's own origin; block plugins, base-URL
	// injection, foreign form posts, and all framing.
	{"Content-Security-Policy", "default-src 'self'; form-action 'self'; base-uri 'self'; object-src 'none'; frame-ancestors 'none'"},
	// Never guess content types, so disguised HTML cannot execute.
	{"X-Content-Type-Options", "nosniff"},
	// Framing fallback for browsers without CSP frame-ancestors.
	{"X-Frame-Options", "DENY"},
	// Do not leak the current URL to other sites.
	{"Referrer-Policy", "no-referrer"},
	// Disable the listed browser features; unlisted features keep their defaults.
	{"Permissions-Policy", "geolocation=(), camera=(), microphone=(), payment=(), usb=()"},
	// Prevent cross-origin opener access; this affects OAuth and popup flows.
	{"Cross-Origin-Opener-Policy", "same-origin"},
	// Prevent cross-origin embedding, including intentional cross-origin assets.
	{"Cross-Origin-Resource-Policy", "same-origin"},
	// Refuse Flash and PDF cross-domain policy lookups.
	{"X-Permitted-Cross-Domain-Policies", "none"},
	// Do not leak visited hostnames through DNS prefetching.
	{"X-DNS-Prefetch-Control", "off"},
}

const hstsHeader = "Strict-Transport-Security"

type secureHeaders struct {
	overrides         map[string]string
	omit              map[string]bool
	hsts              bool
	hstsMaxAge        int64
	includeSubDomains bool
	forceHSTS         bool
}

// SecureHeadersOption configures SecureHeaders.
type SecureHeadersOption func(*secureHeaders)

func override(name, value string) SecureHeadersOption {
	mustValidValue(name, value)
	key := http.CanonicalHeaderKey(name)
	return func(s *secureHeaders) { s.overrides[key] = value }
}

// WithContentSecurityPolicy replaces the Content-Security-Policy value.
func WithContentSecurityPolicy(v string) SecureHeadersOption {
	return override("Content-Security-Policy", v)
}

// WithReferrerPolicy replaces the Referrer-Policy value.
func WithReferrerPolicy(v string) SecureHeadersOption {
	return override("Referrer-Policy", v)
}

// WithPermissionsPolicy replaces the Permissions-Policy value.
func WithPermissionsPolicy(v string) SecureHeadersOption {
	return override("Permissions-Policy", v)
}

// WithCrossOriginOpenerPolicy replaces the Cross-Origin-Opener-Policy value.
func WithCrossOriginOpenerPolicy(v string) SecureHeadersOption {
	return override("Cross-Origin-Opener-Policy", v)
}

// WithCrossOriginResourcePolicy replaces the Cross-Origin-Resource-Policy value.
func WithCrossOriginResourcePolicy(v string) SecureHeadersOption {
	return override("Cross-Origin-Resource-Policy", v)
}

// WithXFrameOptions replaces the X-Frame-Options value.
func WithXFrameOptions(v string) SecureHeadersOption {
	return override("X-Frame-Options", v)
}

// WithHSTS enables Strict-Transport-Security for two years on TLS requests.
func WithHSTS() SecureHeadersOption {
	return func(s *secureHeaders) { s.hsts = true }
}

// WithHSTSMaxAge sets the HSTS lifetime; zero withdraws the policy and
// negative values panic.
func WithHSTSMaxAge(d time.Duration) SecureHeadersOption {
	if d < 0 {
		panic("middleware.WithHSTSMaxAge: negative duration")
	}
	seconds := int64(d / time.Second)
	return func(s *secureHeaders) {
		s.hsts = true
		s.hstsMaxAge = seconds
	}
}

// WithHSTSIncludeSubDomains enables HSTS for this host and all subdomains; use
// it only when every subdomain serves HTTPS.
func WithHSTSIncludeSubDomains() SecureHeadersOption {
	return func(s *secureHeaders) {
		s.hsts = true
		s.includeSubDomains = true
	}
}

// WithForceHSTS enables HSTS behind a TLS-terminating proxy that redirects
// HTTP to HTTPS.
func WithForceHSTS() SecureHeadersOption {
	return func(s *secureHeaders) {
		s.hsts = true
		s.forceHSTS = true
	}
}

// WithoutHeader drops one of the supported headers from the set.
func WithoutHeader(name string) SecureHeadersOption {
	key := http.CanonicalHeaderKey(name)
	if !supportedHeader(key) {
		panic(fmt.Sprintf("middleware.WithoutHeader: unsupported header %q", name))
	}
	return func(s *secureHeaders) { s.omit[key] = true }
}

func supportedHeader(canonical string) bool {
	if canonical == http.CanonicalHeaderKey(hstsHeader) {
		return true
	}
	for _, d := range secureDefaults {
		if http.CanonicalHeaderKey(d[0]) == canonical {
			return true
		}
	}
	return false
}

// mustValidValue accepts nonblank printable ASCII and tabs but rejects HTTP obs-text.
func mustValidValue(name, value string) {
	trimmed := false
	for i := 0; i < len(value); i++ {
		b := value[i]
		if b != ' ' && b != '\t' {
			trimmed = true
		}
		if (b < 0x20 && b != '\t') || b > 0x7e {
			panic(fmt.Sprintf("middleware.SecureHeaders: %s value contains invalid byte %#x", name, b))
		}
	}
	if !trimmed {
		panic(fmt.Sprintf("middleware.SecureHeaders: empty %s value", name))
	}
}

// SecureHeaders sets security headers before the handler runs so handlers can
// override them; HSTS is opt-in, and obsolete X-XSS-Protection is never sent.
func SecureHeaders(opts ...SecureHeadersOption) func(http.Handler) http.Handler {
	cfg := secureHeaders{
		overrides:  map[string]string{},
		omit:       map[string]bool{},
		hstsMaxAge: 63072000,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	hstsValue := "max-age=" + strconv.FormatInt(cfg.hstsMaxAge, 10)
	if cfg.includeSubDomains {
		hstsValue += "; includeSubDomains"
	}
	hstsKey := http.CanonicalHeaderKey(hstsHeader)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			for _, d := range secureDefaults {
				key := http.CanonicalHeaderKey(d[0])
				if cfg.omit[key] {
					continue
				}
				value := d[1]
				if o, ok := cfg.overrides[key]; ok {
					value = o
				}
				h.Set(d[0], value)
			}
			if cfg.hsts && !cfg.omit[hstsKey] && (r.TLS != nil || cfg.forceHSTS) {
				h.Set(hstsHeader, hstsValue)
			}
			next.ServeHTTP(w, r)
		})
	}
}
