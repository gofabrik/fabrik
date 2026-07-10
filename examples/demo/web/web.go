package web

import (
	"database/sql"
	"log/slog"
	"time"

	"github.com/gofabrik/fabrik/web"
)

var started = time.Now()

// HomePage is the landing page view model.
type HomePage struct {
	Greeting string
	Started  time.Time
	Visits   int64
}

func (HomePage) Template() string { return "web/home" }

type Handlers struct {
	Greeter Greeter
	DB      *sql.DB
}

//fabrik:web GET /{$} middleware=nocache
func (h *Handlers) Index(req *web.Request) (web.Response, error) {
	name := req.Query("name")
	if name == "" {
		name = "world"
	}
	slog.InfoContext(req.Context(), "greeting", "name", name)
	var visits int64
	err := h.DB.QueryRowContext(req.Context(),
		`INSERT INTO visits (id, count) VALUES (1, 1)
		 ON CONFLICT (id) DO UPDATE SET count = count + 1
		 RETURNING count`).Scan(&visits)
	if err != nil {
		return nil, err
	}
	return web.View(HomePage{Greeting: h.Greeter.Greet(name), Started: started, Visits: visits}), nil
}
