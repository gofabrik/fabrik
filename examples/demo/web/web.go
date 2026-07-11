package web

import (
	"database/sql"
	"log/slog"
	"time"

	"github.com/gofabrik/fabrik/query"
	"github.com/gofabrik/fabrik/web"
)

var started = time.Now()

// HomePage is the landing page view model.
type HomePage struct {
	Greeting string
	Started  time.Time
	Visits   int64
	Recent   []Greeting
}

// Greeting is one recorded visit, a query row struct.
type Greeting struct {
	ID      int64
	Name    string
	Created time.Time
}

// visitCount reads the counter upsert's RETURNING value - a scalar
// wrapped in a one-field struct, the query read convention.
type visitCount struct {
	Count int64
}

func (HomePage) Template() string { return "web/home" }

type Handlers struct {
	Greeter Greeter
	DB      *sql.DB
	Dialect query.Dialect
}

//fabrik:web GET /{$} middleware=nocache
func (h *Handlers) Index(req *web.Request) (web.Response, error) {
	name := req.Query("name")
	if name == "" {
		name = "world"
	}

	slog.InfoContext(req.Context(), "greeting", "name", name)

	ctx := req.Context()
	visits, err := query.One[visitCount](ctx, h.DB,
		`INSERT INTO visits (id, count) VALUES (1, 1)
		 ON CONFLICT (id) DO UPDATE SET count = count + 1
		 RETURNING count`)
	if err != nil {
		return nil, err
	}
	if _, err := query.Insert(ctx, h.DB, h.Dialect, "greetings", Greeting{Name: name, Created: time.Now()}); err != nil {
		return nil, err
	}

	recent, err := query.All[Greeting](ctx, h.DB,
		"SELECT * FROM greetings ORDER BY id DESC LIMIT 5")
	if err != nil {
		return nil, err
	}

	return web.View(HomePage{Greeting: h.Greeter.Greet(name), Started: started, Visits: visits.Count, Recent: recent}), nil
}
