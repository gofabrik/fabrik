package auth

import (
	sessionauth "github.com/gofabrik/fabrik/authn/session"
	"github.com/gofabrik/fabrik/session"
)

//fabrik:provider
func NewSessionAuth(m *session.Manager) (*sessionauth.Auth, error) {
	return sessionauth.New(m, sessionauth.Options{})
}
