package auth

import (
	"log/slog"
	"net/http"

	fabrikauth "github.com/gofabrik/fabrik/auth"
)

// Identify runs inside the session middleware; authenticator errors degrade
// to anonymous.
//
//fabrik:http:middleware insert=last
func Identify(a fabrikauth.Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, err := a.Authenticate(r)
			if err != nil {
				slog.ErrorContext(r.Context(), "auth: authenticate failed", "error", err)
				id = nil
			}
			next.ServeHTTP(w, r.WithContext(fabrikauth.NewContext(r.Context(), id)))
		})
	}
}
