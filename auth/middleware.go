package auth

import (
	"context"
	"errors"
	"net/http"
	"reflect"
)

// identityCtxKey is unexported so the only way to read the Identity
// from a request context is via [FromContext]. One global key is
// fine - Identity is the same type everywhere and the auth package
// owns the namespace.
type identityCtxKey struct{}

// FromContext returns the [Identity] stashed by [Required] or
// [Optional]. The second return is false when no identity is
// present - typically because Optional ran on an anonymous request.
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityCtxKey{}).(Identity)
	return id, ok
}

// Required runs the authenticator on each request. On success it
// stashes the Identity in the request context (retrievable via
// [FromContext]) and calls the next handler. On [ErrUnauthenticated]
// it writes 401 and stops. Any other error (store outage, etc.)
// writes 500 - a misbehaving authenticator should fail closed, not
// silently let requests through.
//
// The return type is the raw middleware func, assignable to
// router.Middleware and accepted by fabrik's middleware constructor
// form, so the core stays free of any router dependency.
func Required(a Authenticator) func(http.Handler) http.Handler {
	if a == nil || isNilValue(a) {
		panic("auth: Required called with nil Authenticator")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, err := a.Authenticate(r)
			if err != nil {
				if errors.Is(err, ErrUnauthenticated) {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				http.Error(w, "authentication error", http.StatusInternalServerError)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityCtxKey{}, id)))
		})
	}
}

// Optional runs the authenticator but does not require it to
// succeed. On success the Identity goes into the request context
// (retrievable via [FromContext]); on any failure - an unauthenticated
// request or an operational error like a store outage - the request
// continues anonymously.
//
// Optional grants no authorization of its own; it only enriches the
// request when an identity is present. Everything gated sits behind
// [Required] on the protected sub-tree, which still fails closed (401
// or 500) on the same error. So degrading Optional to anonymous on a
// backend blip keeps public routes serving instead of 500-ing a page
// that never needed auth, and opens no bypass - the protected tree is
// unaffected. The tradeoff is that Optional does not itself surface an
// operational auth error; rely on [Required] and on the
// authenticator's own logging (e.g. a password [Sink]) for that.
//
// Use Optional on the broad part of your router (so handlers can
// surface a logged-in name), and layer [Required] on the protected
// sub-tree (e.g. /api).
func Optional(a Authenticator) func(http.Handler) http.Handler {
	if a == nil || isNilValue(a) {
		panic("auth: Optional called with nil Authenticator")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if id, err := a.Authenticate(r); err == nil {
				r = r.WithContext(context.WithValue(r.Context(), identityCtxKey{}, id))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// isNilValue reports a typed-nil interface value (var a *Impl = nil,
// or a nil AuthenticatorFunc), which is non-nil as an interface but
// panics when used. Covers every nil-able kind.
func isNilValue(v any) bool {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Func, reflect.Pointer, reflect.Map, reflect.Slice, reflect.Chan, reflect.Interface:
		return rv.IsNil()
	}
	return false
}
