package auth

import "net/http"

// Authenticator identifies a request. A nil Identity and nil error mean the
// method abstains. Errors leave the request outcome to the caller.
type Authenticator interface {
	Authenticate(r *http.Request) (*Identity, error)
}
