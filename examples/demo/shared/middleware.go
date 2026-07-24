package shared

import (
	"errors"
	"net/http"

	"github.com/gofabrik/fabrik/assetmapper"
	"github.com/gofabrik/fabrik/router/middleware"
	"github.com/gofabrik/fabrik/session"
)

//fabrik:http:middleware
func Logged(next http.Handler) http.Handler { return middleware.Logger(next) }

//fabrik:http:middleware
func Recovered(next http.Handler) http.Handler { return middleware.Recover(next) }

//fabrik:http:middleware
func SecureHeadersMiddleware(assets assetmapper.Runtime, sec *SecurityConfig) (func(http.Handler) http.Handler, error) {
	scriptSrc := []string{middleware.CSPSelf}
	src, ok := assets.ImportmapCSPSource()
	switch {
	case ok:
		scriptSrc = append(scriptSrc, src)
	case sec.AllowUnsafeInline:
		scriptSrc = append(scriptSrc, middleware.CSPUnsafeInline)
	default:
		return nil, errors.New("assets have no build-time script hash; set security.allow_unsafe_inline to serve source assets")
	}
	return middleware.SecureHeaders(
		middleware.WithCSP(middleware.CSP{
			ScriptSrc: scriptSrc,
		}),
	), nil
}

//fabrik:http:middleware
func CrossOriginMiddleware(c *http.CrossOriginProtection) func(http.Handler) http.Handler {
	return c.Handler
}

//fabrik:http:middleware
func SessionMiddleware(m *session.Manager[Session]) func(http.Handler) http.Handler {
	return m.Middleware
}

//fabrik:http:middleware name=nocache
func NoStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
