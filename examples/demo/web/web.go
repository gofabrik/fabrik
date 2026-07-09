package web

import (
	"log/slog"
	"net/http"
)

type Handlers struct {
	Greeter Greeter
}

//fabrik:http GET /{$}
func (h *Handlers) Index(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		name = "world"
	}
	slog.InfoContext(r.Context(), "greeting", "name", name)
	w.Write([]byte(h.Greeter.Greet(name)))
}
