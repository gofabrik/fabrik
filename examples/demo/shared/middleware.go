package shared

import (
	"net/http"

	"github.com/gofabrik/fabrik/assetmapper"
	"github.com/gofabrik/fabrik/router/middleware"
	"github.com/gofabrik/fabrik/session"
)

// Foundation logs requests outside panic recovery and runs before unmarked middleware.
//
//fabrik:http:middleware insert=first
func Foundation(next http.Handler) http.Handler {
	return middleware.Logger(middleware.Recover(next))
}

//fabrik:http:middleware
func SecureHeadersMiddleware(assets assetmapper.Server) func(http.Handler) http.Handler {
	return middleware.SecureHeaders(
		middleware.WithCSP(middleware.CSP{
			ScriptSrc: append([]string{middleware.CSPSelf}, assets.ImportmapCSPSources()...),
		}),
	)
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
