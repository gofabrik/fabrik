package auth

import "net/http"

type headerAuthenticator struct{}

func (headerAuthenticator) Authenticate(r *http.Request) (*ClaimSet, error) {
	if sub := r.Header.Get("X-Subject"); sub != "" {
		return &ClaimSet{Subject: sub}, nil
	}
	return nil, nil
}

var _ Authenticator = headerAuthenticator{}
