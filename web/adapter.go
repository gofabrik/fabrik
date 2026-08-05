package web

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"sync"
)

// Renderer writes a named template with data. The adapter passes the same
// renderer to every Template response.
//
// block names what to execute within name, for a renderer whose templates
// hold more than one thing: a whole document and the fragments of it that a
// swap replaces. It is [WithBlock]'s value unless the response overrides it,
// empty when neither chose, and each renderer owns what empty means:
// [Templates] renders its default entry, and a renderer with nothing to
// choose ignores it.
type Renderer interface {
	Render(w io.Writer, name, block string, data any) error
}

// ErrorHandler handles adapter and response failures.
type ErrorHandler func(http.ResponseWriter, *http.Request, error)

// ErrNilResponse reports a handler that returned nil, nil.
var ErrNilResponse = errors.New("web: handler returned nil response and nil error")

// ErrNoTemplateRenderer reports a Template response on an adapter with no renderer.
var ErrNoTemplateRenderer = errors.New("web: Template response without a renderer (configure WithRenderer)")

// Adapter adapts typed handlers to net/http.
type Adapter struct {
	renderer Renderer
	block    string
	onError  ErrorHandler
}

// Option configures an Adapter.
type Option func(*Adapter)

// WithBlock sets the block every rendered response asks for unless it says
// otherwise, replacing the renderer's own default.
func WithBlock(name string) Option {
	return func(a *Adapter) { a.block = name }
}

// WithRenderer supplies the adapter's renderer.
func WithRenderer(r Renderer) Option { return func(a *Adapter) { a.renderer = r } }

// WithErrorHandler replaces the default handler, which logs errors and returns [ErrorStatus] or HTTP 500.
func WithErrorHandler(h ErrorHandler) Option { return func(a *Adapter) { a.onError = h } }

// NewAdapter returns an Adapter with the given options.
func NewAdapter(opts ...Option) *Adapter {
	a := &Adapter{onError: defaultErrorHandler}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// ErrorStatus returns an error's HTTPStatus value from 400 through 599,
// including through wrapping, and calls HTTPStatus at most once.
func ErrorStatus(err error) (int, bool) {
	var se interface{ HTTPStatus() int }
	if !errors.As(err, &se) {
		return 0, false
	}
	status := se.HTTPStatus()
	if status < 400 || status > 599 {
		return 0, false
	}
	return status, true
}

// applyErrorHeaders sets the headers an error carries, including through
// wrapping. It runs before the error handler, so a handler that answers
// differently is still free to overwrite them.
func applyErrorHeaders(h http.Header, err error) {
	var he interface{ ResponseHeaders() []headerPair }
	if !errors.As(err, &he) {
		return
	}
	for _, pair := range he.ResponseHeaders() {
		h.Set(pair.key, pair.value)
	}
}

func defaultErrorHandler(w http.ResponseWriter, r *http.Request, err error) {
	if status, ok := ErrorStatus(err); ok {
		if status < 500 {
			slog.InfoContext(r.Context(), "web: request rejected", "method", r.Method, "path", r.URL.Path, "status", status, "error", err)
		} else {
			slog.ErrorContext(r.Context(), "web: handler failed", "method", r.Method, "path", r.URL.Path, "status", status, "error", err)
		}
		text := http.StatusText(status)
		if text == "" {
			// Extension codes (499, 599) have no standard text.
			text = "request error"
		}
		http.Error(w, text, status)
		return
	}
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
			maps.Copy(h, snapshot)
			// After the restore, so a rejection that has something to say
			// about its own response still says it. A response's headers are
			// still dropped: these belong to the error, not to what failed.
			applyErrorHeaders(h, err)
			a.onError(cw, r, err)
		}
	}
}

// respond handles renderer-backed responses through the adapter.
func (a *Adapter) respond(w http.ResponseWriter, r *http.Request, resp Response) error {
	if v, ok := resp.(TemplateResponse); ok {
		if a.renderer == nil {
			return ErrNoTemplateRenderer
		}
		return a.render(w, v.name, a.blockFor(v.block), v.status, v.headers, v.data)
	}
	return resp.Respond(w, r)
}

func (a *Adapter) blockFor(block string) string {
	if block != "" {
		return block
	}
	return a.block
}

// render buffers before it writes.
//
// A status has to be chosen before any byte reaches the client, and whether a
// render succeeds is not known until it has run. Rendering into a buffer first
// is what lets a failure reach the error handler as a clean status rather than
// arriving under one already sent.
func (a *Adapter) render(w http.ResponseWriter, name, block string, status int, headers []headerPair, data any) error {
	buf := renderBuffers.Get().(*bytes.Buffer)
	defer func() {
		if buf.Cap() <= maxPooledBuffer {
			buf.Reset()
			renderBuffers.Put(buf)
		}
	}()

	if err := a.renderer.Render(buf, name, block, data); err != nil {
		return fmt.Errorf("web: render %s: %w", name, err)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	applyHeaders(w, headers)
	if status != 0 {
		w.WriteHeader(status)
	}
	_, err := buf.WriteTo(w)
	return err
}

// maxPooledBuffer keeps unusually large render buffers out of the pool.
const maxPooledBuffer = 64 << 10

var renderBuffers = sync.Pool{New: func() any { return new(bytes.Buffer) }}

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
