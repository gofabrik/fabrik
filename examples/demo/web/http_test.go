package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/storage"
)

type failingOpenStorage struct {
	storage.Storage
	err error
}

func (s failingOpenStorage) Open(context.Context, string) (io.ReadCloser, error) {
	return nil, s.err
}

func TestFilesServeHidesStorageErrors(t *testing.T) {
	storeErr := errors.New("dial backend at secret.internal: connection refused")
	files := &Files{Store: failingOpenStorage{Storage: storage.NewMemory(), err: storeErr}}
	req := httptest.NewRequest(http.MethodGet, "/files/uploads/report.pdf", nil)
	req.SetPathValue("key", "uploads/report.pdf")
	rec := httptest.NewRecorder()

	files.Serve(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if strings.Contains(rec.Body.String(), storeErr.Error()) {
		t.Fatalf("response exposed storage error: %q", rec.Body.String())
	}
	if rec.Body.String() != "internal server error\n" {
		t.Fatalf("body = %q, want generic error", rec.Body.String())
	}
}
