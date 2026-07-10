package web

import (
	"errors"
	"log/slog"
	"net/http"
)

// Renderer renders a named template with data.
type Renderer interface {
	Render(w http.ResponseWriter, name string, data any) error
}

// ErrorHandler handles adapter and response failures.
type ErrorHandler func(http.ResponseWriter, *http.Request, error)

// ErrNilResponse reports a handler that returned nil, nil.
var ErrNilResponse = errors.New("web: handler returned nil response and nil error")

// Adapter adapts typed handlers to net/http.
type Adapter struct {
	renderer Renderer
	onError  ErrorHandler
}

// Option configures an Adapter.
type Option func(*Adapter)

// WithRenderer supplies the renderer View responses use.
func WithRenderer(r Renderer) Option { return func(a *Adapter) { a.renderer = r } }

// WithErrorHandler replaces the default slog and plain-500 handler.
func WithErrorHandler(h ErrorHandler) Option { return func(a *Adapter) { a.onError = h } }

// NewAdapter returns an Adapter with the given options.
func NewAdapter(opts ...Option) *Adapter {
	a := &Adapter{onError: defaultErrorHandler}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

func defaultErrorHandler(w http.ResponseWriter, r *http.Request, err error) {
	slog.ErrorContext(r.Context(), "web: handler failed", "method", r.Method, "path", r.URL.Path, "error", err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

// Wrap adapts a typed handler to http.HandlerFunc.
//
// Handler errors skip recorded headers and cookies. Pre-commit response
// errors restore the prior header map before calling the error handler.
// Post-commit errors are logged.
func (a *Adapter) Wrap(fn func(*Request) (Response, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req := newRequest(r)
		resp, err := fn(req)
		if err != nil {
			a.onError(w, r, err)
			return
		}
		if resp == nil {
			a.onError(w, r, ErrNilResponse)
			return
		}
		cw := &commitWriter{ResponseWriter: w}
		var rw http.ResponseWriter = cw
		if _, ok := w.(http.Flusher); ok {
			// Preserve Flusher only when the underlying writer supports it.
			rw = flushWriter{cw}
		}
		// Restore this snapshot on pre-commit response errors.
		snapshot := make(http.Header, len(cw.Header()))
		for key, values := range cw.Header() {
			snapshot[key] = append([]string(nil), values...)
		}
		for key, value := range req.headers {
			rw.Header().Set(key, value)
		}
		for _, c := range req.cookies {
			http.SetCookie(rw, c)
		}
		if err := a.respond(rw, r, resp); err != nil {
			if cw.committed {
				slog.ErrorContext(r.Context(), "web: response failed after commit",
					"method", r.Method, "path", r.URL.Path, "error", err)
				return
			}
			h := cw.Header()
			for key := range h {
				delete(h, key)
			}
			for key, values := range snapshot {
				h[key] = values
			}
			a.onError(cw, r, err)
		}
	}
}

// respond handles renderer-backed responses through the adapter.
func (a *Adapter) respond(w http.ResponseWriter, r *http.Request, resp Response) error {
	if v, ok := resp.(renderResponse); ok {
		if a.renderer == nil {
			return errors.New("web: View/Template response without a renderer (configure WithRenderer)")
		}
		return a.renderer.Render(w, v.name, v.data)
	}
	return resp.Respond(w, r)
}

// commitWriter tracks whether status or body bytes have been written.
type commitWriter struct {
	http.ResponseWriter
	committed bool
}

func (c *commitWriter) WriteHeader(code int) {
	c.committed = true
	c.ResponseWriter.WriteHeader(code)
}

func (c *commitWriter) Write(b []byte) (int, error) {
	c.committed = true
	return c.ResponseWriter.Write(b)
}

// flushWriter exposes Flush only when the underlying writer supports it.
type flushWriter struct{ *commitWriter }

func (f flushWriter) Flush() {
	f.committed = true
	f.ResponseWriter.(http.Flusher).Flush()
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (c *commitWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }
