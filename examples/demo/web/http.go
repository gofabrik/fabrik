package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime"
	"strconv"
	"time"

	"demo/shared"

	"github.com/gofabrik/fabrik/cache"
	"github.com/gofabrik/fabrik/flash"
	"github.com/gofabrik/fabrik/forms"
	"github.com/gofabrik/fabrik/jobs"
	"github.com/gofabrik/fabrik/paging"
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
}

// Greeting is a recorded greeting.
type Greeting struct {
	ID      int64
	Name    string
	Created time.Time
}

type Handlers struct {
	Greeter Greeter
	Queries *query.DB
	Session *session.Manager[shared.Session]
	Flash   *flash.Flash
	Jobs    *jobs.Manager
	Cache   *cache.Cache[[]Greeting]
}

//fabrik:web GET /{$} middleware=nocache
func (h *Handlers) Index(req *web.Request) (web.Response, error) {
	ctx := req.Context()

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
	visits, err := query.Scalar[int64](ctx, h.Queries,
		`SELECT COALESCE((SELECT count FROM visits WHERE id = 1), 0)`)
	if err != nil {
		return nil, err
	}

	recent, err := h.Cache.GetOrLoad(ctx, "recent", 10*time.Second,
		func(ctx context.Context) ([]Greeting, error) {
			return query.All[Greeting](ctx, h.Queries,
				"SELECT * FROM greetings ORDER BY id DESC LIMIT 5")
		})
	if err != nil {
		return nil, err
	}

	return web.Template("web/home", HomePage{Greeting: h.Greeter.Greet(name), Started: started, Visits: visits, Recent: recent}), nil
}

// AboutPage is the about page's view model; it carries no template name.
type AboutPage struct {
	Started   time.Time
	GoVersion string
}

//fabrik:web GET /about
func (h *Handlers) About(req *web.Request) (web.Response, error) {
	return web.Template("web/about", AboutPage{Started: started, GoVersion: runtime.Version()}), nil
}

// UptimePage is the uptime page's view model.
type UptimePage struct {
	Started time.Time
}

// Status renders through the template set directly, without the web adapter.
type Status struct {
	Templates *web.Templates
}

//fabrik:http GET /uptime
func (s *Status) Uptime(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.Templates.Render(w, "web/uptime", "", UptimePage{Started: started}); err != nil {
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
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

type Greetings struct {
	Session *session.Manager[shared.Session]
	Flash   *flash.Flash
	Queries *query.DB
	Jobs    *jobs.Manager
	Cache   *cache.Cache[[]Greeting]
}

//fabrik:web GET /greet
func (h *Greetings) Show(req *web.Request) (web.Response, error) {
	return web.Template("web/greet", GreetForm{Form: forms.Empty[GreetInput]()}), nil
}

//fabrik:web POST /greet middleware=greetlimit
func (h *Greetings) Update(req *web.Request) (web.Response, error) {
	form, err := forms.Bind[GreetInput](req.HTTP())
	if err != nil {
		return nil, err
	}
	if !form.Valid() {
		return web.Template("web/greet", GreetForm{Form: form}), nil
	}
	ctx := req.Context()
	if err := h.Session.Save(ctx, shared.Session{Name: form.Data.Name}); err != nil {
		return nil, err
	}
	if _, err := h.Queries.Insert(ctx, "greetings", Greeting{Name: form.Data.Name, Created: time.Now()}); err != nil {
		return nil, err
	}
	if err := h.Cache.Delete(ctx, "recent"); err != nil {
		slog.WarnContext(ctx, "greeting cache delete failed", "error", err)
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

type UploadForm struct {
	File forms.File `form:"file"`
}

func (u UploadForm) Validate() validation.Errors {
	return validation.Check(
		validation.Field("file", u.File, validation.By(func(f forms.File) error {
			if !f.Present() {
				return errors.New("choose a file")
			}
			return nil
		})),
	)
}

type FilesPage struct {
	Entries []storage.Info
	Form    *forms.Form[UploadForm]
}

type Files struct {
	Store storage.Storage
}

func (f *Files) page(req *web.Request, form *forms.Form[UploadForm]) (FilesPage, error) {
	var entries []storage.Info
	for info, err := range f.Store.List(req.Context(), "") {
		if err != nil {
			return FilesPage{}, err
		}
		entries = append(entries, info)
	}
	return FilesPage{Entries: entries, Form: form}, nil
}

//fabrik:web GET /files
func (f *Files) Show(req *web.Request) (web.Response, error) {
	page, err := f.page(req, forms.Empty[UploadForm]())
	if err != nil {
		return nil, err
	}
	return web.Template("web/files", page), nil
}

//fabrik:web POST /files
func (f *Files) Upload(req *web.Request) (web.Response, error) {
	form, err := forms.Bind[UploadForm](req.HTTP(), forms.WithMaxBytes(maxUpload))
	if err != nil {
		return nil, err
	}
	if !form.Valid() {
		page, err := f.page(req, form)
		if err != nil {
			return nil, err
		}
		return web.Template("web/files", page), nil
	}
	rc, err := form.Data.File.Open()
	if err != nil {
		return nil, err
	}
	key := storage.UniqueKey("uploads", form.Data.File.ClientFilename())
	putErr := f.Store.Put(req.Context(), key, rc)
	closeErr := rc.Close()
	if err := errors.Join(putErr, closeErr); err != nil {
		return nil, err
	}
	return web.Redirect("/files"), nil
}

// OverviewPage carries every region's data for the overview page.
type OverviewPage struct {
	Recent  []Greeting
	Files   []storage.Info
	Started time.Time
}

type Overview struct {
	Queries *query.DB
	Store   storage.Storage
}

//fabrik:web GET /overview
func (o *Overview) Show(req *web.Request) (web.Response, error) {
	ctx := req.Context()
	recent, err := query.All[Greeting](ctx, o.Queries,
		"SELECT * FROM greetings ORDER BY id DESC LIMIT 5")
	if err != nil {
		return nil, err
	}
	var files []storage.Info
	for info, err := range o.Store.List(ctx, "") {
		if err != nil {
			return nil, err
		}
		files = append(files, info)
	}
	return web.Template("web/overview", OverviewPage{Recent: recent, Files: files, Started: started}).Fragment(), nil
}

// GreetingsPage is the paged greetings list view model.
type GreetingsPage struct {
	Rows []Greeting
	Page paging.Page
}

type GreetingsList struct {
	Queries *query.DB
}

//fabrik:web GET /greetings
func (l *GreetingsList) Show(req *web.Request) (web.Response, error) {
	ctx := req.Context()
	page, _ := strconv.Atoi(req.Query("page"))
	total, err := query.Scalar[int](ctx, l.Queries, "SELECT COUNT(*) FROM greetings")
	if err != nil {
		return nil, err
	}
	found := paging.Of(page, 5, total)
	rows, err := query.All[Greeting](ctx, l.Queries,
		"SELECT * FROM greetings ORDER BY id DESC LIMIT ? OFFSET ?", found.Limit(), found.Offset())
	if err != nil {
		return nil, err
	}
	return web.Template("web/greetings", GreetingsPage{Rows: rows, Page: found}).Fragment(), nil
}

// GreetingEditPage is the edit form's view model.
type GreetingEditPage struct {
	ID   int64
	Form *forms.Form[GreetInput]
}

// greetingName is the writable column set for a greeting update.
type greetingName struct {
	Name string
}

type GreetingEditor struct {
	Queries *query.DB
}

//fabrik:web GET /greetings/{id}/edit
func (e *GreetingEditor) Edit(req *web.Request) (web.Response, error) {
	ctx := req.Context()
	id, err := strconv.ParseInt(req.PathValue("id"), 10, 64)
	if err != nil {
		return web.Status(http.StatusNotFound), nil
	}
	g, err := query.One[Greeting](ctx, e.Queries, "SELECT * FROM greetings WHERE id = ? LIMIT 1", id)
	if err != nil {
		if errors.Is(err, query.ErrNotFound) {
			return web.Status(http.StatusNotFound), nil
		}
		return nil, err
	}
	return web.Template("web/greeting-edit", GreetingEditPage{ID: g.ID, Form: forms.From(GreetInput{Name: g.Name})}), nil
}

//fabrik:web POST /greetings/{id}/edit
func (e *GreetingEditor) Update(req *web.Request) (web.Response, error) {
	ctx := req.Context()
	id, err := strconv.ParseInt(req.PathValue("id"), 10, 64)
	if err != nil {
		return web.Status(http.StatusNotFound), nil
	}
	form, err := forms.Bind[GreetInput](req.HTTP())
	if err != nil {
		return nil, err
	}
	if !form.Valid() {
		return web.Template("web/greeting-edit", GreetingEditPage{ID: id, Form: form}).Status(http.StatusUnprocessableEntity), nil
	}
	if err := e.Queries.UpdateOne(ctx, "greetings", "id = ?", greetingName{Name: form.Data.Name}, id); err != nil {
		if errors.Is(err, query.ErrNotFound) {
			return web.Status(http.StatusNotFound), nil
		}
		return nil, err
	}
	return web.Redirect("/greetings"), nil
}

type Live struct {
	Queries *query.DB
}

//fabrik:web GET /live
func (l *Live) Show(req *web.Request) (web.Response, error) {
	return web.Template("web/live", nil), nil
}

func (l *Live) line(ctx context.Context) (string, error) {
	visits, err := query.Scalar[int64](ctx, l.Queries,
		`SELECT COALESCE((SELECT count FROM visits WHERE id = 1), 0)`)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Up %s, %d visits", time.Since(started).Round(time.Second), visits), nil
}

//fabrik:web GET /live/events
func (l *Live) Events(req *web.Request) (web.Response, error) {
	initial, err := l.line(req.Context())
	if err != nil {
		return nil, err
	}

	// The stream owns the ticker goroutine. OnClose cancels streamCtx however
	// the stream ends, so the goroutine always returns; both sends race that
	// cancellation, so a departed client never blocks it and events is never
	// closed under a pending send.
	streamCtx, stop := context.WithCancel(req.Context())
	events := make(chan string)
	go func() {
		// The sole sender, so closing here is safe and ends the stream when
		// a tick fails rather than leaving the response waiting forever.
		defer close(events)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-streamCtx.Done():
				return
			case <-ticker.C:
				line, err := l.line(streamCtx)
				if err != nil {
					return
				}
				select {
				case events <- line:
				case <-streamCtx.Done():
					return
				}
			}
		}
	}()

	return web.EventStream(events).Initial(initial).OnClose(stop), nil
}

//fabrik:http GET /files/{key...}
func (f *Files) Serve(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if err := storage.CheckKey(key); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	rc, err := f.Store.Open(r.Context(), key)
	if err != nil {
		if errors.Is(err, storage.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		slog.ErrorContext(r.Context(), "file open failed", "key", key, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	defer func() {
		if err := rc.Close(); err != nil {
			slog.WarnContext(r.Context(), "file close failed", "key", key, "error", err)
		}
	}()
	// Force downloads so untrusted uploads cannot render as same-origin content.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment")
	rs, ok := rc.(io.ReadSeeker)
	if !ok {
		if _, err := io.Copy(w, rc); err != nil {
			slog.DebugContext(r.Context(), "file response write failed", "key", key, "error", err)
		}
		return
	}
	// A zero modtime avoids a racy Stat of a potentially newer version.
	http.ServeContent(w, r, key, time.Time{}, rs)
}
