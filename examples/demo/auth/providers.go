// Package auth wires the demo's authentication from the batteries the
// library ships: a SQLite user store, the session bridge, the chain,
// and the server-rendered UI. No store SQL, templates, ID/email
// helpers, or redirect logic live here - the library owns all of it.
package auth

import (
	"database/sql"

	fauth "github.com/gofabrik/fabrik/auth"
	"github.com/gofabrik/fabrik/auth/session"
	sqlstore "github.com/gofabrik/fabrik/auth/store/sqlite"
	authweb "github.com/gofabrik/fabrik/auth/web"
	"github.com/gofabrik/fabrik/session"

	"demo/shared"
)

//fabrik:provider
func NewUsers(db *sql.DB) (*sqlstore.Store, error) {
	return sqlstore.New(db, sqlstore.Options{AutoCreate: true})
}

//fabrik:provider
func NewSessionAuth(m *session.Manager[shared.Session]) (*authsession.Authenticator, error) {
	return authsession.New(m)
}

//fabrik:provider
func NewAuthChain(sa *authsession.Authenticator) fauth.Authenticator {
	return fauth.Chain(sa)
}

//fabrik:provider
func NewAuthUI(users *sqlstore.Store, sa *authsession.Authenticator, chain fauth.Authenticator) (*authweb.UI, error) {
	return authweb.New(users, sa, authweb.Options{
		Auth:      fauth.Required(chain), // protect the account page
		RateLimit: shared.RateLimit,      // throttle the credential POSTs
		// everything else is the library default
	})
}
