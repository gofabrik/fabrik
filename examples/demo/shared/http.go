package shared

import (
	"net/http"
	"time"

	"github.com/gofabrik/fabrik/templates"
)

//fabrik:provider
func NewServer(cfg *Config) *http.Server {
	return &http.Server{
		Addr:              cfg.Addr,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

// ErrorPages renders the router's miss responses through the app's
// template set. Body-only handlers keep the router's status codes.
type ErrorPages struct {
	Templates *templates.Set
}

//fabrik:http:notfound
func (e *ErrorPages) NotFound(w http.ResponseWriter, r *http.Request) {
	if err := e.Templates.Render(w, "errors/404", map[string]any{"Path": r.URL.Path}); err != nil {
		http.NotFound(w, r)
	}
}

//fabrik:http:methodnotallowed
func (e *ErrorPages) MethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{"Method": r.Method, "Path": r.URL.Path}
	if err := e.Templates.Render(w, "errors/405", data); err != nil {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
