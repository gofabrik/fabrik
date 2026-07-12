package auth

import (
	"net/http"

	authweb "github.com/gofabrik/fabrik/auth/web"
)

// Mount hands the auth UI's routes to the router in one line - the
// produces-handler form registers its internal mux under /auth/.
type Mount struct {
	UI *authweb.UI
}

//fabrik:http:handle /auth/
func (m *Mount) Handler() http.Handler {
	return m.UI.Handler()
}
