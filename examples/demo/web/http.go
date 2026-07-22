package web

import (
	"demo/shared"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/gofabrik/fabrik/flash"
	"github.com/gofabrik/fabrik/forms"
	"github.com/gofabrik/fabrik/jobs"
	"github.com/gofabrik/fabrik/query"
	"github.com/gofabrik/fabrik/router"
	"github.com/gofabrik/fabrik/session"
	"github.com/gofabrik/fabrik/validation"
	"github.com/gofabrik/fabrik/web"
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

// Greeting is a recorded greeting.
type Greeting struct {
	ID      int64
	Name    string
	Created time.Time
}

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

	s, err := h.Session.Get(ctx)
	if err != nil {
		return nil, err
	}
	name := s.Name
	if name == "" {
		name = "world"
	}

	slog.InfoContext(ctx, "greeting", "name", name)

	// Visit counts lag because workers persist them asynchronously.
	if _, err := h.Jobs.Enqueue(ctx, Visit{Path: "/"}); err != nil {
		return nil, err
	}
	visits, err := query.One[visitCount](ctx, h.Queries,
		`SELECT COALESCE((SELECT count FROM visits WHERE id = 1), 0) AS count`)
	if err != nil {
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
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(a.Greeter.Greet(r.PathValue("name")))) // #nosec G705 -- served as text/plain (Content-Type above), so not interpreted as HTML
}

// GreetInput is the greeting-name form.
type GreetInput struct {
	Name string
}

func (in GreetInput) Validate() validation.Errors {
	return validation.Check(
		validation.Field("name", in.Name, validation.Required(), validation.MaxLen(20)),
	)
}

// GreetForm is the greeting form's view model.
type GreetForm struct {
	Form *forms.Form[GreetInput]
}

func (GreetForm) Template() string { return "web/greet" }

type Greetings struct {
	Session *session.Manager[shared.Session]
	Flash   *flash.Flash
	Queries *query.DB
	Jobs    *jobs.Manager
}

//fabrik:web GET /greet
func (h *Greetings) Show(req *web.Request) (web.Response, error) {
	return web.View(GreetForm{Form: &forms.Form[GreetInput]{}}), nil
}

//fabrik:web POST /greet middleware=ratelimit
func (h *Greetings) Update(req *web.Request) (web.Response, error) {
	form, err := forms.Bind[GreetInput](req.HTTP())
	if err != nil {
		if errors.Is(err, forms.ErrBodyTooLarge) {
			return web.Status(http.StatusRequestEntityTooLarge), nil
		}
		return web.Status(http.StatusBadRequest), nil
	}
	if !form.Valid() {
		return web.View(GreetForm{Form: form}), nil
	}
	ctx := req.Context()
	if err := h.Session.Save(ctx, shared.Session{Name: form.Data.Name}); err != nil {
		return nil, err
	}
	if _, err := h.Queries.Insert(ctx, "greetings", Greeting{Name: form.Data.Name, Created: time.Now()}); err != nil {
		return nil, err
	}
	if err := h.Flash.Add(ctx, "success", "Greeting name updated."); err != nil {
		return nil, err
	}
	if _, err := h.Jobs.Enqueue(ctx, shared.GreetingNotification{Name: form.Data.Name}); err != nil {
		return nil, err
	}
	return web.Redirect("/"), nil
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
		fmt.Fprintf(w, "%s %s\n", method, rt.Pattern) //nolint:errcheck // response write; nothing to do on client disconnect
	}
}
