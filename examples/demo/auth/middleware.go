package auth

import (
	"net/http"
	"time"

	"github.com/gofabrik/fabrik/authn"
	"github.com/gofabrik/fabrik/authn/session"
	"github.com/gofabrik/fabrik/ratelimit"
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
	return authn.Guard{OnUnauthenticated: on401}.Authenticated()
}

//fabrik:http:middleware name=admin requires=authenticated
func Admin(adapter *web.Adapter) func(http.Handler) http.Handler {
	on401 := adapter.Wrap(func(req *web.Request) (web.Response, error) {
		return web.Template("errors/401", ErrorPage{Status: 401}).Status(401), nil
	})
	on403 := adapter.Wrap(func(req *web.Request) (web.Response, error) {
		return web.Template("errors/403", ErrorPage{Status: 403}).Status(403), nil
	})
	return authn.Guard{OnUnauthenticated: on401, OnForbidden: on403}.Role("admin")
}

//fabrik:http:middleware name=loginlimit
func LoginRateLimited(store *ratelimit.MemoryStore) (func(http.Handler) http.Handler, error) {
	l, err := ratelimit.New(ratelimit.Limit{Rate: 30, Period: time.Minute, Burst: 20}, store, ratelimit.WithNamespace("login-ip"))
	if err != nil {
		return nil, err
	}
	return ratelimit.Middleware(l, ratelimit.WithFailClosed()), nil
}
