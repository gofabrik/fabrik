package shared

import (
	"net/http"

	"github.com/gofabrik/fabrik/router/middleware"
)

//fabrik:http:middleware
func Logged(next http.Handler) http.Handler { return middleware.Logger(next) }

//fabrik:http:middleware
func Recovered(next http.Handler) http.Handler { return middleware.Recover(next) }
