package shared

import (
	"github.com/gofabrik/fabrik/web"
)

type ErrorPages struct{}

// NotFoundPage is the 404 page's view model.
type NotFoundPage struct {
	Path string
}

// MethodNotAllowedPage is the 405 page's view model.
type MethodNotAllowedPage struct {
	Method string
	Path   string
}

//fabrik:web:notfound
func (e *ErrorPages) NotFound(req *web.Request) (web.Response, error) {
	r := req.HTTP()
	return web.Template("errors/404", NotFoundPage{Path: r.URL.Path}), nil
}

//fabrik:web:methodnotallowed
func (e *ErrorPages) MethodNotAllowed(req *web.Request) (web.Response, error) {
	r := req.HTTP()
	return web.Template("errors/405", MethodNotAllowedPage{Method: r.Method, Path: r.URL.Path}), nil
}
