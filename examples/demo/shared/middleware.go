package shared

import (
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gofabrik/fabrik/router/middleware"
	"github.com/gofabrik/fabrik/session"
)

// The unnamed middleware below are applied globally in declaration
// order (Logged, Recovered, SessionMiddleware): recovery and logging
// wrap session handling, so keep SessionMiddleware last of the three.

//fabrik:http:middleware
func Logged(next http.Handler) http.Handler { return middleware.Logger(next) }

//fabrik:http:middleware
func Recovered(next http.Handler) http.Handler { return middleware.Recover(next) }

//fabrik:http:middleware
func SessionMiddleware(m *session.Manager[Session]) func(http.Handler) http.Handler {
	return m.Middleware
}

// NoStore disables client caching.
//
//fabrik:http:middleware name=nocache
func NoStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// RateLimit is a minimal real per-IP fixed-window limiter - a plain
// middleware value the auth UI throttles its credential POSTs with
// (web.Options.RateLimit). A production app uses the fabrik
// ratelimit library once it lands, or a distributed limiter; this
// one is in-memory and single-process.
func RateLimit(next http.Handler) http.Handler {
	const (
		limit  = 10
		window = time.Minute
	)
	var mu sync.Mutex
	type bucket struct {
		count int
		reset time.Time
	}
	seen := map[string]*bucket{}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			ip = r.RemoteAddr // no port: use the whole value, not ""
		}
		now := time.Now()

		mu.Lock()
		// Opportunistic pruning keeps the map bounded - fine for a
		// single-process demo.
		for k, v := range seen {
			if now.After(v.reset) {
				delete(seen, k)
			}
		}
		b := seen[ip]
		if b == nil || now.After(b.reset) {
			b = &bucket{reset: now.Add(window)}
			seen[ip] = b
		}
		b.count++
		over := b.count > limit
		mu.Unlock()

		if over {
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}
