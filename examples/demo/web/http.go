package web

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/gofabrik/fabrik/flash"
	"github.com/gofabrik/fabrik/jobs"
	"github.com/gofabrik/fabrik/query"
	"github.com/gofabrik/fabrik/router"
	"github.com/gofabrik/fabrik/session"
	"github.com/gofabrik/fabrik/web"

	"demo/shared"
)

var started = time.Now()

// HomePage is the landing page view model.
type HomePage struct {
	Greeting string
	Started  time.Time
	Visits   int64
	Recent   []Greeting
	Flashes  []flash.Message
}

// Greeting is one recorded visit.
type Greeting struct {
	ID      int64
	Name    string
	Created time.Time
}

// visitCount reads the counter upsert's RETURNING value.
type visitCount struct {
	Count int64
}

func (HomePage) Template() string { return "web/home" }

type Handlers struct {
	Greeter Greeter
	Queries *query.DB
	Session *session.Manager[shared.Session]
	Flash   *flash.Flash
	Jobs    *jobs.Manager
}

//fabrik:web GET /{$} middleware=nocache
func (h *Handlers) Index(req *web.Request) (web.Response, error) {
	ctx := req.Context()

	flashes, err := h.Flash.Take(ctx)
	if err != nil {
		return nil, err
	}

	name := req.Query("name")
	if name != "" {
		if err := h.Session.Save(ctx, shared.Session{Name: name}); err != nil {
			return nil, err
		}
		if err := h.Flash.Add(ctx, "success", "Greeting name updated."); err != nil {
			return nil, err
		}
	} else {
		s, err := h.Session.Get(ctx)
		if err != nil {
			return nil, err
		}
		name = s.Name
		if name == "" {
			name = "world"
		}
	}

	slog.InfoContext(ctx, "greeting", "name", name)

	// Recording the visit is deferred to a background job; the page reads
	// the running count without waiting on the write.
	if _, err := h.Jobs.Enqueue(ctx, Visit{Path: "/"}); err != nil {
		return nil, err
	}
	visits, err := query.One[visitCount](ctx, h.Queries,
		`SELECT COALESCE((SELECT count FROM visits WHERE id = 1), 0) AS count`)
	if err != nil {
		return nil, err
	}
	if _, err := h.Queries.Insert(ctx, "greetings", Greeting{Name: name, Created: time.Now()}); err != nil {
		return nil, err
	}

	recent, err := query.All[Greeting](ctx, h.Queries,
		"SELECT * FROM greetings ORDER BY id DESC LIMIT 5")
	if err != nil {
		return nil, err
	}

	return web.View(HomePage{Greeting: h.Greeter.Greet(name), Started: started, Visits: visits.Count, Recent: recent, Flashes: flashes}), nil
}

//fabrik:http:group /api
type API struct {
	Greeter Greeter
}

//fabrik:http GET /greet/{name}
func (a *API) Greet(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte(a.Greeter.Greet(r.PathValue("name"))))
}

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
