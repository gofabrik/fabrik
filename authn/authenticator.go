package authn

import "net/http"

// Authenticator verifies request credentials. Authenticate returns
// (nil, nil) when the request has no credential for the scheme and a
// non-nil error when a presented credential is invalid.
type Authenticator interface {
	Authenticate(r *http.Request) (*ClaimSet, error)
}
