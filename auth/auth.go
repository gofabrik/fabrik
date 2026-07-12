// Package auth is a pluggable authentication library built around one
// idea: a request either has an identity or it does not, and the rest
// of the app should not care how that identity was established.
//
// Two primitives drive composition:
//
//   - [Authenticator] is the function shape backends implement to
//     identify a request. forwardauth, the session bridge, and
//     future backends all satisfy it.
//   - [Chain] composes Authenticators into one. First in the chain
//     to return a non-error wins; [ErrUnauthenticated] falls
//     through; any other error short-circuits.
//
// [Required] and [Optional] turn an Authenticator into middleware:
// Required 401s on a miss, Optional passes anonymously, both fail
// closed (500) on a non-sentinel error. Their wire behavior is
// deliberately fixed - an app wanting redirect-to-login or JSON
// errors writes its own middleware around Authenticate and
// [FromContext], which are the real API.
//
// The core depends only on the standard library. Backends that
// persist identity own that coupling in their own packages.
package auth

import (
	"errors"
	"fmt"
	"net/http"
)

// Identity is the authenticated principal for a request. It is safe
// to log, cache, or pass as a context value. The Claims map makes
// value-comparison via == invalid; use [reflect.DeepEqual] or
// compare individual fields.
type Identity struct {
	// Subject is a stable opaque user ID, unique within Provider.
	// Two providers may use the same Subject string for different
	// humans; do not compare Subjects across Providers.
	Subject string

	// Email and Name are best-effort attributes the Provider
	// surfaced. Either may be empty.
	Email string
	Name  string

	// Provider names the backend that produced this Identity:
	// "password", "forward", etc. Match against known constants in
	// your code; the auth library never interprets this field.
	Provider string

	// Claims is a free-form map for provider-specific extras (OIDC
	// claims, forward-auth header values, scopes). Apps reading from
	// it must trust the Provider that put values there.
	Claims map[string]any
}

// Authenticator extracts an [Identity] from a request. Return
// [ErrUnauthenticated] to indicate "no identity here" - the chain
// then falls through to the next authenticator. Any other error
// short-circuits the chain so a store outage does not accidentally
// let a request fall through to a less-trusted authenticator.
type Authenticator interface {
	Authenticate(r *http.Request) (Identity, error)
}

// AuthenticatorFunc is the func adapter for [Authenticator]. Use it
// to register inline authenticators without declaring a new type.
type AuthenticatorFunc func(r *http.Request) (Identity, error)

// Authenticate implements [Authenticator].
func (f AuthenticatorFunc) Authenticate(r *http.Request) (Identity, error) { return f(r) }

// ErrUnauthenticated is the sentinel for "no identity here". Return
// it from [Authenticator.Authenticate] to fall through to the next
// authenticator in a [Chain]; the request reaches [Required]'s 401
// or [Optional]'s anonymous pass-through only after every chained
// authenticator has fallen through.
var ErrUnauthenticated = errors.New("auth: unauthenticated")

// Chain composes multiple [Authenticator]s into one. The returned
// Authenticator tries each in registration order; the first to
// return without [ErrUnauthenticated] wins, any other error
// short-circuits. An empty chain always returns [ErrUnauthenticated].
//
// Panics if any element is nil, with the offending index in the
// message - the same fail-loud-at-construction discipline as
// [Required] and [Optional]. A nil entry slipping through would
// otherwise panic deep inside a request handler the first time it is
// exercised, which is much harder to diagnose.
//
// Typical use puts per-request authenticators (forward-auth header,
// bearer token) before session-reading backends so an API client
// with a token is not mistaken for a stale browser session.
func Chain(as ...Authenticator) Authenticator {
	cp := make([]Authenticator, len(as))
	copy(cp, as)
	for i, a := range cp {
		if a == nil || isNilValue(a) {
			panic(fmt.Sprintf("auth: Chain called with nil Authenticator at index %d", i))
		}
	}
	return AuthenticatorFunc(func(r *http.Request) (Identity, error) {
		for _, a := range cp {
			id, err := a.Authenticate(r)
			if err == nil {
				return id, nil
			}
			if !errors.Is(err, ErrUnauthenticated) {
				return Identity{}, err
			}
		}
		return Identity{}, ErrUnauthenticated
	})
}
