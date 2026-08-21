package session

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type deadlineProbeStore struct {
	inner       *MemoryStore
	hadDeadline bool
}

func (s *deadlineProbeStore) Load(ctx context.Context, sid string) (Record, error) {
	return s.inner.Load(ctx, sid)
}

func (s *deadlineProbeStore) Save(ctx context.Context, rec Record) (Record, error) {
	_, s.hadDeadline = ctx.Deadline()
	return s.inner.Save(ctx, rec)
}

func (s *deadlineProbeStore) Delete(ctx context.Context, sid string) error {
	return s.inner.Delete(ctx, sid)
}

type blockingStore struct {
	inner *MemoryStore
}

func (s *blockingStore) Load(ctx context.Context, sid string) (Record, error) {
	return s.inner.Load(ctx, sid)
}

func (s *blockingStore) Save(ctx context.Context, rec Record) (Record, error) {
	<-ctx.Done()
	return Record{}, ctx.Err()
}

func (s *blockingStore) Delete(ctx context.Context, sid string) error {
	return s.inner.Delete(ctx, sid)
}

func saveHandler(t *testing.T, h *Handle[appSession]) func(http.ResponseWriter, *http.Request) {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if err := h.Save(r.Context(), appSession{Name: "v"}); err != nil {
			t.Errorf("save: %v", err)
		}
	}
}

func TestCommitBoundedByTimeout(t *testing.T) {
	store := &blockingStore{inner: NewMemoryStore(MemoryOptions{})}
	m := newTestManager(t, func(c *Config) {
		c.Store = store
		c.MaxRetries = -1
		c.CommitTimeout = 50 * time.Millisecond
	})
	h := appH(m)

	done := make(chan *int, 1)
	go func() {
		rr := serve(t, m, "", saveHandler(t, h))
		code := rr.Code
		done <- &code
	}()
	select {
	case code := <-done:
		if *code != http.StatusInternalServerError {
			t.Fatalf("bounded commit = %d, want 500", *code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("commit not bounded: still blocked after 2s with CommitTimeout 50ms")
	}
}

func TestCommitTimeoutDefaultsTo30s(t *testing.T) {
	m := newTestManager(t)
	if m.commitTimeout != 30*time.Second {
		t.Fatalf("resolved commitTimeout = %v, want 30s", m.commitTimeout)
	}
}

func TestCommitTimeoutAppliesDeadline(t *testing.T) {
	store := &deadlineProbeStore{inner: NewMemoryStore(MemoryOptions{})}
	m := newTestManager(t, func(c *Config) {
		c.Store = store
		c.CommitTimeout = 5 * time.Second
	})
	h := appH(m)
	serve(t, m, "", saveHandler(t, h))
	if !store.hadDeadline {
		t.Fatal("commit context must carry a deadline with a positive CommitTimeout")
	}
}

func TestCommitTimeoutNegativeDisables(t *testing.T) {
	store := &deadlineProbeStore{inner: NewMemoryStore(MemoryOptions{})}
	m := newTestManager(t, func(c *Config) {
		c.Store = store
		c.CommitTimeout = -1
	})
	h := appH(m)
	serve(t, m, "", saveHandler(t, h))
	if store.hadDeadline {
		t.Fatal("negative CommitTimeout must disable the commit deadline")
	}
}

// CommitTimeout bounds store work, not handler execution.
func TestSlowHandlerStillCommits(t *testing.T) {
	store := &deadlineProbeStore{inner: NewMemoryStore(MemoryOptions{})}
	m := newTestManager(t, func(c *Config) {
		c.Store = store
		c.CommitTimeout = 75 * time.Millisecond
	})
	h := appH(m)
	rr := serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		if err := h.Save(r.Context(), appSession{Name: "v"}); err != nil {
			t.Errorf("save: %v", err)
		}
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("slow handler commit = %d, want 200", rr.Code)
	}
	if rr.Header().Get("Set-Cookie") == "" {
		t.Fatal("slow handler commit produced no session cookie")
	}
}

type flushErrBase struct {
	http.ResponseWriter
	feErr error
}

func (b *flushErrBase) Flush() {}

func (b *flushErrBase) FlushError() error { return b.feErr }

// ResponseController prefers FlushError, so wrappers must preserve its error.
func TestFlushErrorPropagatesThroughMiddleware(t *testing.T) {
	m := newTestManager(t)
	feErr := errors.New("flush refused")
	var got error
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = http.NewResponseController(w).Flush()
	})
	rec := &flushErrBase{ResponseWriter: httptest.NewRecorder(), feErr: feErr}
	m.Middleware(inner).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if got != feErr {
		t.Fatalf("ResponseController.Flush = %v, want the base error by identity", got)
	}
}

type feOnlyBase struct {
	http.ResponseWriter
	feErr error
}

func (b *feOnlyBase) FlushError() error { return b.feErr }

func TestFlushErrorOnlyBaseThroughMiddleware(t *testing.T) {
	m := newTestManager(t)
	feErr := errors.New("flush refused")
	var got error
	var advertised bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, advertised = w.(http.Flusher)
		got = http.NewResponseController(w).Flush()
	})
	rec := &feOnlyBase{ResponseWriter: httptest.NewRecorder(), feErr: feErr}
	m.Middleware(inner).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if !advertised {
		t.Fatal("FlushError-only base must still advertise flushing through the wrapper")
	}
	if got != feErr {
		t.Fatalf("ResponseController.Flush = %v, want the base error by identity", got)
	}
}
