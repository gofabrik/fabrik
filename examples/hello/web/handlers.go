package web

import "net/http"

// Handlers holds the HTTP handlers and their wired dependencies.
type Handlers struct {
	Greeter *Greeter
}

//fabrik:http GET /
func (h *Handlers) Index(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		name = "world"
	}
	w.Write([]byte(h.Greeter.Greet(name)))
}
