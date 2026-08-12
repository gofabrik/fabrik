package auth

import (
	fabrikauth "github.com/gofabrik/fabrik/auth"
	authsession "github.com/gofabrik/fabrik/auth/session"
	"github.com/gofabrik/fabrik/session"

	"demo/shared"
)

//fabrik:provider
func NewAuthenticator(m *session.Manager[shared.Session]) (fabrikauth.Authenticator, error) {
	return authsession.New(m)
}
