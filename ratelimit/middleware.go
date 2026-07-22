package ratelimit

import (
	"math"
	"net"
	"net/http"
	"strconv"
)

// KeyFunc derives a request's limit key; an empty key is degraded and passes
// through unless [WithFailClosed] is set.
type KeyFunc func(*http.Request) string

// KeyByIP returns the host from RemoteAddr, or RemoteAddr unchanged if it
// cannot be split; trusted proxies should use a KeyFunc that reads only
// trusted headers.
func KeyByIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

type middlewareOpts struct {
	key        KeyFunc
	failClosed bool
	limited    http.Handler
}

// MiddlewareOption configures [Middleware].
type MiddlewareOption func(*middlewareOpts)

// WithKeyFunc sets the key derivation; the default is [KeyByIP].
func WithKeyFunc(k KeyFunc) MiddlewareOption { return func(o *middlewareOpts) { o.key = k } }

// WithFailClosed returns 503 for empty keys or store errors instead of passing
// requests through.
func WithFailClosed() MiddlewareOption { return func(o *middlewareOpts) { o.failClosed = true } }

// WithLimitedHandler customizes quota denials; Middleware preserves status 429
// and does not invoke the handler for degraded requests.
func WithLimitedHandler(h http.Handler) MiddlewareOption {
	return func(o *middlewareOpts) { o.limited = h }
}

// forced429 prevents limited handlers from changing the denial status.
type forced429 struct {
	http.ResponseWriter
	wrote bool
}

func (w *forced429) WriteHeader(int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.ResponseWriter.WriteHeader(http.StatusTooManyRequests)
}

func (w *forced429) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusTooManyRequests)
	}
	return w.ResponseWriter.Write(b)
}

// Middleware wraps handlers with keyed rate limiting; genuine denials return
// 429 with RateLimit-Limit, RateLimit-Remaining, RateLimit-Reset, and a
// rounded-up Retry-After, while degraded requests pass through by default or
// return a headerless 503 with [WithFailClosed].
func Middleware(l *Limiter, opts ...MiddlewareOption) func(http.Handler) http.Handler {
	o := middlewareOpts{key: KeyByIP}
	for _, opt := range opts {
		opt(&o)
	}
	if o.limited == nil {
		o.limited = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		})
	}
	degraded := func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		if o.failClosed {
			http.Error(w, "rate limiter unavailable", http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(w, r)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			k := o.key(r)
			if k == "" {
				degraded(w, r, next)
				return
			}
			res, err := l.Allow(r.Context(), k)
			if err != nil {
				degraded(w, r, next)
				return
			}
			w.Header().Set("RateLimit-Limit", strconv.Itoa(res.Limit))
			w.Header().Set("RateLimit-Remaining", strconv.Itoa(res.Remaining))
			w.Header().Set("RateLimit-Reset", strconv.Itoa(int(math.Ceil(res.ResetAfter.Seconds()))))
			if !res.Allowed {
				w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(res.RetryAfter.Seconds()))))
				fw := &forced429{ResponseWriter: w}
				o.limited.ServeHTTP(fw, r)
				if !fw.wrote {
					fw.WriteHeader(http.StatusTooManyRequests)
				}
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
