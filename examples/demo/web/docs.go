package web

import (
	"fmt"
	"net/http"

	"github.com/gofabrik/fabrik/router"
)

// Docs lists the registered routes.
type Docs struct {
	Router *router.Router
}

//fabrik:http GET /routes
func (d *Docs) List(w http.ResponseWriter, r *http.Request) {
	for _, rt := range d.Router.Routes() {
		method := rt.Method
		if method == "" {
			method = "ANY"
		}
		fmt.Fprintf(w, "%s %s\n", method, rt.Pattern)
	}
}
