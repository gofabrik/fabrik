package auth

import (
	"net/http"

	fauth "github.com/gofabrik/fabrik/auth"
)

// OptionalAuth surfaces identity when present without gating - the
// home page uses it. RequireAuth is no longer a named middleware:
// the auth UI protects its own account page via web.Options.Auth.
//
//fabrik:http:middleware name=optauth
func OptionalAuth(c fauth.Authenticator) func(http.Handler) http.Handler {
	return fauth.Optional(c)
}
