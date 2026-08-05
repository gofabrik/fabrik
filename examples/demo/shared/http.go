package shared

import (
	"bytes"
	"log/slog"
	"net/http"

	"github.com/gofabrik/fabrik/web"
)

type ErrorPages struct {
	Templates *web.Templates
}

// NotFoundPage is the 404 page's view model.
type NotFoundPage struct {
	Path string
}

// MethodNotAllowedPage is the 405 page's view model.
type MethodNotAllowedPage struct {
	Method string
	Path   string
}

//fabrik:http:notfound
func (e *ErrorPages) NotFound(w http.ResponseWriter, r *http.Request) {
	var page bytes.Buffer
	if err := e.Templates.Render(&page, "errors/404", "", NotFoundPage{Path: r.URL.Path}); err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := page.WriteTo(w); err != nil {
		slog.DebugContext(r.Context(), "not-found response write failed", "error", err)
	}
}

//fabrik:http:methodnotallowed
func (e *ErrorPages) MethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	data := MethodNotAllowedPage{Method: r.Method, Path: r.URL.Path}
	var page bytes.Buffer
	if err := e.Templates.Render(&page, "errors/405", "", data); err != nil {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := page.WriteTo(w); err != nil {
		slog.DebugContext(r.Context(), "method-not-allowed response write failed", "error", err)
	}
}
