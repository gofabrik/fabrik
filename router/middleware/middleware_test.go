package middleware

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
