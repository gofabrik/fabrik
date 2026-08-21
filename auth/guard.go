package auth

import "net/http"

// Guard builds route middleware that enforces claims. The zero value
// responds with bare statuses: 401 when the request has no
// authenticated principal, 403 when the principal lacks the required
// grant. OnUnauthenticated and OnForbidden override those responses;
// each receives the original request.
type Guard struct {
	OnUnauthenticated http.Handler
	OnForbidden       http.Handler
}

// Authenticated requires an authenticated principal.
func (g Guard) Authenticated() func(http.Handler) http.Handler {
	return g.require(func(r *http.Request) bool { return true })
}

// Role requires an authenticated principal with role r.
func (g Guard) Role(r string) func(http.Handler) http.Handler {
	return g.require(func(req *http.Request) bool { return HasRole(req.Context(), r) })
}

// Scope requires an authenticated principal with scope s.
func (g Guard) Scope(s string) func(http.Handler) http.Handler {
	return g.require(func(req *http.Request) bool { return HasScope(req.Context(), s) })
}

func (g Guard) require(granted func(*http.Request) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !IsAuthenticated(r.Context()) {
				g.unauthenticated(w, r)
				return
			}
			if !granted(r) {
				g.forbidden(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (g Guard) unauthenticated(w http.ResponseWriter, r *http.Request) {
	if g.OnUnauthenticated != nil {
		g.OnUnauthenticated.ServeHTTP(w, r)
		return
	}
	http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
}

func (g Guard) forbidden(w http.ResponseWriter, r *http.Request) {
	if g.OnForbidden != nil {
		g.OnForbidden.ServeHTTP(w, r)
		return
	}
	http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
}
