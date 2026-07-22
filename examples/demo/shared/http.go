package shared

import (
	"bytes"
	"net/http"

	"github.com/gofabrik/fabrik/templates"
)

type ErrorPages struct {
	Templates *templates.Set
}

//fabrik:http:notfound
func (e *ErrorPages) NotFound(w http.ResponseWriter, r *http.Request) {
	var page bytes.Buffer
	if err := e.Templates.Render(&page, "errors/404", map[string]any{"Path": r.URL.Path}); err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	page.WriteTo(w)
}

//fabrik:http:methodnotallowed
func (e *ErrorPages) MethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{"Method": r.Method, "Path": r.URL.Path}
	var page bytes.Buffer
	if err := e.Templates.Render(&page, "errors/405", data); err != nil {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	page.WriteTo(w)
}
