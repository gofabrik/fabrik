package shared

import (
	"net/http"

	"github.com/gofabrik/fabrik/router/middleware"
	"github.com/gofabrik/fabrik/session"
)

//fabrik:http:middleware
func Logged(next http.Handler) http.Handler { return middleware.Logger(next) }

//fabrik:http:middleware
func Recovered(next http.Handler) http.Handler { return middleware.Recover(next) }

//fabrik:http:middleware
func CrossOriginMiddleware(c *http.CrossOriginProtection) func(http.Handler) http.Handler {
	return c.Handler
}

//fabrik:http:middleware
func SessionMiddleware(m *session.Manager[Session]) func(http.Handler) http.Handler {
	return m.Middleware
}

//
//fabrik:http:middleware name=nocache
func NoStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
