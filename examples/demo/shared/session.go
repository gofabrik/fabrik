package shared

import (
	"net/http"

	"github.com/gofabrik/fabrik/session"
)

// Session is the app's session data: the greeting name persists per
// visitor until renamed.
type Session struct {
	Name string
}

//fabrik:http:middleware
func SessionMiddleware(m *session.Manager[Session]) func(http.Handler) http.Handler {
	return m.Middleware
}
