package web

import (
	"log/slog"
	"time"

	"github.com/gofabrik/fabrik/web"
)

var started = time.Now()

// HomePage is the landing page view model.
type HomePage struct {
	Greeting string
	Started  time.Time
}

func (HomePage) Template() string { return "web/home" }

type Handlers struct {
	Greeter Greeter
}

//fabrik:web GET /{$} middleware=nocache
func (h *Handlers) Index(req *web.Request) (web.Response, error) {
	name := req.Query("name")
	if name == "" {
		name = "world"
	}
	slog.InfoContext(req.Context(), "greeting", "name", name)
	return web.View(HomePage{Greeting: h.Greeter.Greet(name), Started: started}), nil
}
