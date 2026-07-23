package shared

import (
	"crypto/sha256"
	"encoding/base64"
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
func SecureHeadersMiddleware(assets *assetmapper.Compiled) (func(http.Handler) http.Handler, error) {
	parts, err := assets.RenderImportmap(assetmapper.RenderOptions{Entrypoints: []string{"app"}})
	if err != nil {
		return nil, err
	}
	// Build-time hashes allow only the page's known inline scripts.
	csp := "default-src 'self'; script-src 'self'"
	for _, s := range parts.InlineScripts {
		sum := sha256.Sum256([]byte(s))
		csp += " 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	}
	csp += "; form-action 'self'; base-uri 'self'; object-src 'none'; frame-ancestors 'none'"
	return middleware.SecureHeaders(middleware.WithContentSecurityPolicy(csp)), nil
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
