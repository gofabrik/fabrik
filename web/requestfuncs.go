package web

import (
	"fmt"
	"io"
	"net/http"
)

// RequestFuncs maps template names to per-request constructors, each of which
// must return a template func with one result, optionally followed by an error;
// do not mutate the map after template loading or adapter configuration.
type RequestFuncs map[string]func(*http.Request) any

// requestStub distinguishes request funcs from static helpers during loading.
type requestStub func(args ...any) (any, error)

// Stubs returns placeholders that reserve request-func names during loading.
func (rf RequestFuncs) Stubs() FuncMap {
	stubs := make(FuncMap, len(rf))
	for name := range rf {
		stubs[name] = requestStub(func(args ...any) (any, error) {
			return nil, fmt.Errorf("web: template func %q needs a request", name)
		})
	}
	return stubs
}

// For returns funcs bound to r, or nil when rf is empty; nil constructors
// produce funcs that fail during rendering.
func (rf RequestFuncs) For(r *http.Request) FuncMap {
	if len(rf) == 0 {
		return nil
	}
	m := make(FuncMap, len(rf))
	for name, build := range rf {
		if build == nil {
			m[name] = func(args ...any) (any, error) {
				return nil, fmt.Errorf("web: request func %q has a nil constructor", name)
			}
			continue
		}
		m[name] = build(r)
	}
	return m
}

// MergeRequestFuncs combines several RequestFuncs; later names win.
func MergeRequestFuncs(rfs ...RequestFuncs) RequestFuncs {
	merged := RequestFuncs{}
	for _, rf := range rfs {
		for name, build := range rf {
			merged[name] = build
		}
	}
	return merged
}

// RequestInfo identifies an HTTP request in templates.
type RequestInfo struct {
	Method string
	Path   string
	Host   string
}

// DefaultRequestFuncs returns the built-in request funcs:
//
//	request          -> RequestInfo{Method, Path, Host}
//	pathValue "name" -> the named route wildcard value
//	query "name"     -> the named URL query value
//
// Cookies and form values are omitted: templates must not read session
// cookies, and submitted values belong to form binding.
func DefaultRequestFuncs() RequestFuncs {
	return RequestFuncs{
		"request": func(r *http.Request) any {
			return func() RequestInfo {
				return RequestInfo{Method: r.Method, Path: r.URL.Path, Host: r.Host}
			}
		},
		"pathValue": func(r *http.Request) any {
			return func(name string) string { return r.PathValue(name) }
		},
		"query": func(r *http.Request) any {
			return func(name string) string { return r.URL.Query().Get(name) }
		},
	}
}

// FuncsRenderer renders with a per-execution func overlay.
type FuncsRenderer interface {
	Renderer
	RenderFuncs(w io.Writer, name, block string, data any, funcs FuncMap) error
}

// ErrRendererNoFuncs reports a renderer that cannot apply configured request funcs.
var ErrRendererNoFuncs = fmt.Errorf("web: request funcs configured but the renderer cannot overlay funcs (see FuncsRenderer)")

// RequestRenderer renders with request funcs outside an [Adapter].
type RequestRenderer struct {
	Templates *Templates
	Funcs     RequestFuncs
}

// Render renders a template with funcs bound to r.
func (rr RequestRenderer) Render(w io.Writer, r *http.Request, page, define string, data any) error {
	return rr.Templates.RenderFuncs(w, page, define, data, rr.Funcs.For(r))
}
