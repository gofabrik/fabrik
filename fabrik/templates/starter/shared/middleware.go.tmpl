package shared

import (
	"net/http"

	"github.com/gofabrik/fabrik/router/middleware"
)

//fabrik:http:middleware
func Logged(next http.Handler) http.Handler { return middleware.Logger(next) }

//fabrik:http:middleware
func Recovered(next http.Handler) http.Handler { return middleware.Recover(next) }

// NoStore disables client caching.
//
//fabrik:http:middleware name=nocache
func NoStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
