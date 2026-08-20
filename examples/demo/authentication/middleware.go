package authentication

import (
	"net/http"

	"github.com/gofabrik/fabrik/auth"
	"github.com/gofabrik/fabrik/auth/session"
	"github.com/gofabrik/fabrik/web"
)

//fabrik:http:middleware name=sessionauth global=true requires=session
func SessionAuthMiddleware(a *session.Auth) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		attach := a.Middleware(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Rendered authentication state varies by session, so shared caches vary by cookie.
			w.Header().Add("Vary", "Cookie")
			attach.ServeHTTP(w, r)
		})
	}
}

//fabrik:http:middleware name=authenticated
func Authenticated(adapter *web.Adapter) func(http.Handler) http.Handler {
	on401 := adapter.Wrap(func(req *web.Request) (web.Response, error) {
		return web.Template("errors/401", ErrorPage{Status: 401}).Status(401), nil
	})
	return auth.Guard{OnUnauthenticated: on401}.Authenticated()
}

//fabrik:http:middleware name=admin requires=authenticated
func Admin(adapter *web.Adapter) func(http.Handler) http.Handler {
	on401 := adapter.Wrap(func(req *web.Request) (web.Response, error) {
		return web.Template("errors/401", ErrorPage{Status: 401}).Status(401), nil
	})
	on403 := adapter.Wrap(func(req *web.Request) (web.Response, error) {
		return web.Template("errors/403", ErrorPage{Status: 403}).Status(403), nil
	})
	return auth.Guard{OnUnauthenticated: on401, OnForbidden: on403}.Role("admin")
}
