package shared

import (
	"net/http"

	"github.com/gofabrik/fabrik/assets"
	"github.com/gofabrik/fabrik/router/middleware"
	"github.com/gofabrik/fabrik/session"
)

// LogAndRecover logs requests outside panic recovery and runs before unmarked middleware.
//
//fabrik:http:middleware global=true before=*
func LogAndRecover(next http.Handler) http.Handler {
	return middleware.Logger(middleware.Recover(next))
}

//fabrik:http:middleware global=true
func SecureHeadersMiddleware(assets assets.Server) func(http.Handler) http.Handler {
	return middleware.SecureHeaders(
		middleware.WithCSP(middleware.CSP{
			ScriptSrc: append([]string{middleware.CSPSelf}, assets.ImportmapCSPSources()...),
		}),
	)
}

//fabrik:http:middleware global=true
func CrossOriginMiddleware(c *http.CrossOriginProtection) func(http.Handler) http.Handler {
	return c.Handler
}

//fabrik:http:middleware name=session global=true
func SessionMiddleware(m *session.Manager) func(http.Handler) http.Handler {
	return m.Middleware
}

//fabrik:http:middleware name=nocache
func NoStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
