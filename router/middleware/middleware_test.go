package middleware

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type syncWriter struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

type hijackWriter struct {
	*httptest.ResponseRecorder
	hijacked      bool
	writeAttempts int
}

func (h *hijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.hijacked = true
	c1, c2 := net.Pipe()
	c2.Close()
	rw := bufio.NewReadWriter(bufio.NewReader(c1), bufio.NewWriter(c1))
	return c1, rw, nil
}

func (h *hijackWriter) WriteHeader(code int) {
	if h.hijacked {
		h.writeAttempts++
	}
	h.ResponseRecorder.WriteHeader(code)
}

func TestRequestID(t *testing.T) {
	var inCtx string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inCtx = RequestIDFrom(r.Context())
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	header := rec.Header().Get("X-Request-Id")
	if header == "" || header != inCtx {
		t.Errorf("header id %q, context id %q; want equal and non-empty", header, inCtx)
	}
}

func TestLoggerCapturesStatus(t *testing.T) {
	h := Logger(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusTeapot)
	}
}

func TestRecover(t *testing.T) {
	h := Recover(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestRecoverAbortPassesThrough(t *testing.T) {
	h := Recover(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer func() {
		if recover() != http.ErrAbortHandler {
			t.Error("http.ErrAbortHandler did not pass through")
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
}

// Logger must observe the 500 produced when Recover handles a panic.
func TestLoggerLogsRecoveredPanic(t *testing.T) {
	prev := slog.Default()
	defer slog.SetDefault(prev)
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))

	h := Logger(Recover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	logged := buf.String()
	if !strings.Contains(logged, "msg=panic") {
		t.Errorf("panic not logged:\n%s", logged)
	}
	if !strings.Contains(logged, "msg=request") || !strings.Contains(logged, "status=500") {
		t.Errorf("request line missing or without the recovered status:\n%s", logged)
	}
}

// TestRecoverCommittedPanic verifies that a committed panic causes a transport
// error instead of a successful truncated response.
func TestRecoverCommittedPanic(t *testing.T) {
	h := Recover(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("partial")) //nolint:errcheck
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic("committed panic")
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}
	defer resp.Body.Close()

	_, readErr := io.ReadAll(resp.Body)
	if !errors.Is(readErr, io.ErrUnexpectedEOF) {
		t.Errorf("client read error = %v, want io.ErrUnexpectedEOF; truncated body must not arrive as complete success", readErr)
	}
}

// TestLoggerRecoverStackedCommittedPanic verifies that Logger emits one request line when Recover aborts after commit.
func TestLoggerRecoverStackedCommittedPanic(t *testing.T) {
	prev := slog.Default()
	defer slog.SetDefault(prev)
	sw := &syncWriter{}
	slog.SetDefault(slog.New(slog.NewTextHandler(sw, nil)))

	h := Logger(Recover(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("partial")) //nolint:errcheck
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic("committed panic")
	})))
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err == nil {
		io.ReadAll(resp.Body) //nolint:errcheck
		resp.Body.Close()
	}

	logged := sw.String()
	if n := strings.Count(logged, "msg=request"); n != 1 {
		t.Errorf("request log line count = %d, want 1:\n%s", n, logged)
	}
	if n := strings.Count(logged, "msg=panic"); n != 1 {
		t.Errorf("panic log line count = %d, want 1:\n%s", n, logged)
	}
	if !strings.Contains(logged, "value=\"committed panic\"") || !strings.Contains(logged, "goroutine ") {
		t.Errorf("panic line missing value or stack trace:\n%s", logged)
	}
}

// TestRecoverHijackPanic verifies that a post-hijack panic aborts without writing a 500.
func TestRecoverHijackPanic(t *testing.T) {
	hw := &hijackWriter{ResponseRecorder: httptest.NewRecorder()}

	h := Recover(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, _, err := w.(http.Hijacker).Hijack(); err != nil {
			t.Errorf("hijack failed: %v", err)
			return
		}
		panic("after hijack")
	}))

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		h.ServeHTTP(hw, httptest.NewRequest("GET", "/", nil))
	}()

	if recovered != http.ErrAbortHandler {
		t.Errorf("recovered %v, want http.ErrAbortHandler", recovered)
	}
	if hw.writeAttempts != 0 {
		t.Errorf("WriteHeader called %d time(s) on hijacked writer, want 0", hw.writeAttempts)
	}
}
