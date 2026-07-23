package web

import (
	"demo/shared"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/gofabrik/fabrik/flash"
	"github.com/gofabrik/fabrik/forms"
	"github.com/gofabrik/fabrik/jobs"
	"github.com/gofabrik/fabrik/query"
	"github.com/gofabrik/fabrik/router"
	"github.com/gofabrik/fabrik/session"
	"github.com/gofabrik/fabrik/storage"
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

//fabrik:web POST /greet middleware=greetlimit
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

const maxUpload = 8 << 20

type FilesPage struct {
	Entries []storage.Info
}

func (FilesPage) Template() string { return "web/files" }

type Files struct {
	Store storage.Storage
}

//fabrik:web GET /files
func (f *Files) Show(req *web.Request) (web.Response, error) {
	var entries []storage.Info
	for info, err := range f.Store.List(req.Context(), "") {
		if err != nil {
			return nil, err
		}
		entries = append(entries, info)
	}
	return web.View(FilesPage{Entries: entries}), nil
}

//fabrik:web POST /files
func (f *Files) Upload(req *web.Request) (web.Response, error) {
	r := req.HTTP()
	// Bind enforces both the total request limit and multipart memory limit.
	if _, err := forms.Bind[struct{}](r, forms.WithMaxBytes(maxUpload)); err != nil {
		if errors.Is(err, forms.ErrBodyTooLarge) {
			return web.Status(http.StatusRequestEntityTooLarge), nil
		}
		return web.Status(http.StatusBadRequest), nil
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		return web.Status(http.StatusBadRequest), nil
	}
	defer file.Close()
	key := "uploads/" + header.Filename
	if err := storage.CheckKey(key); err != nil {
		return web.Redirect("/files"), nil
	}
	if err := f.Store.Put(req.Context(), key, file); err != nil {
		return nil, err
	}
	return web.Redirect("/files"), nil
}

//fabrik:http GET /files/{key...}
func (f *Files) Serve(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	rc, err := f.Store.Open(r.Context(), key)
	if err != nil {
		if errors.Is(err, storage.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer rc.Close()
	// Force downloads so untrusted uploads cannot render as same-origin content.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment")
	rs, ok := rc.(io.ReadSeeker)
	if !ok {
		io.Copy(w, rc)
		return
	}
	// A zero modtime avoids a racy Stat of a potentially newer version.
	http.ServeContent(w, r, key, time.Time{}, rs)
}
