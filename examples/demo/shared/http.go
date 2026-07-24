package shared

import (
	"bytes"
	"net/http"

	"github.com/gofabrik/fabrik/session"
	"github.com/gofabrik/fabrik/templates"
)

type ErrorPage struct {
	Method string
	Path   string
}

// ErrorPages renders 404 and 405 pages without consuming pending flashes.
type ErrorPages struct {
	Templates *templates.Set
	Sessions  *session.Manager[Session]
}

func (e *ErrorPages) data(r *http.Request, page any) (*WebData, error) {
	return compose(r, page, e.Sessions)
}

//fabrik:http:notfound
func (e *ErrorPages) NotFound(w http.ResponseWriter, r *http.Request) {
	d, err := e.data(r, ErrorPage{Path: r.URL.Path})
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var page bytes.Buffer
	if err := e.Templates.Render(&page, "errors/404", d); err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	page.WriteTo(w)
}

//fabrik:http:methodnotallowed
func (e *ErrorPages) MethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	d, err := e.data(r, ErrorPage{Method: r.Method, Path: r.URL.Path})
	if err != nil {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var page bytes.Buffer
	if err := e.Templates.Render(&page, "errors/405", d); err != nil {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusMethodNotAllowed)
	page.WriteTo(w)
}
