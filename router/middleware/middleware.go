// Package middleware provides standard-library HTTP middleware.
package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

type ctxKey int

const requestIDKey ctxKey = iota

// RequestID assigns each request a random ID in X-Request-Id and the context.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 8)
		rand.Read(buf)
		id := hex.EncodeToString(buf)
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

// RequestIDFrom returns the request ID stored by RequestID, or "".
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// Logger writes one slog line per request, including when downstream aborts
// after committing a response. Compose it outside Recover, as
// Logger(Recover(next)): the reverse order logs a recovered panic's status
// before Recover writes the 500.
func Logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			attrs := []any{
				"method", r.Method,
				"path", r.URL.Path,
				"status", sw.status,
				"duration", time.Since(start),
			}
			if id := RequestIDFrom(r.Context()); id != "" {
				attrs = append(attrs, "requestId", id)
			}
			slog.InfoContext(r.Context(), "request", attrs...)
		}()
		next.ServeHTTP(wrapStatusWriter(sw), r)
	})
}

// Recover logs handler panics with their stack and writes a 500 before commit;
// committed or hijacked responses abort, and callers must close hijacked connections.
// It re-panics http.ErrAbortHandler.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			p := recover()
			if p == nil {
				return
			}
			if p == http.ErrAbortHandler {
				panic(p)
			}
			slog.ErrorContext(r.Context(), "panic", "value", p, "stack", string(debug.Stack()))
			if sw.wrote {
				panic(http.ErrAbortHandler)
			}
			w.WriteHeader(http.StatusInternalServerError)
		}()
		next.ServeHTTP(wrapStatusWriter(sw), r)
	})
}

// statusWriter records the response status.
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.wrote = true
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
