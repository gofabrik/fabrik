package shared

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/flash"
	"github.com/gofabrik/fabrik/session"
	"github.com/gofabrik/fabrik/web"
)

func manager(t *testing.T, store session.Store) *session.Manager[Session] {
	t.Helper()
	m, err := session.New[Session](session.Config{
		Store:          store,
		Token:          session.Cookie{Name: "demo_session"},
		AbsoluteExpiry: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// requestContext returns the context a handler sees behind the session
// middleware; a non-empty sid arrives as the cookie, so the store is read.
func requestContext(t *testing.T, m *session.Manager[Session], sid string, fn func(ctx context.Context)) context.Context {
	t.Helper()
	var ctx context.Context
	h := m.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		ctx = r.Context()
		if fn != nil {
			fn(ctx)
		}
	}))
	req := httptest.NewRequest("GET", "/", nil)
	if sid != "" {
		req.AddCookie(&http.Cookie{Name: "demo_session", Value: sid})
	}
	h.ServeHTTP(httptest.NewRecorder(), req)
	if ctx == nil {
		t.Fatal("middleware never reached the handler")
	}
	return ctx
}

func TestCurrentUserWithoutMiddleware(t *testing.T) {
	h := &ViewHelpers{Sessions: manager(t, session.NewMemoryStore())}
	user, err := h.CurrentUser(context.Background())
	if err != nil {
		t.Fatalf("a requestless render must not fail: %v", err)
	}
	if user != nil {
		t.Errorf("user = %v, want nil outside a request", user)
	}
}

func TestCurrentUserAnonymous(t *testing.T) {
	m := manager(t, session.NewMemoryStore())
	h := &ViewHelpers{Sessions: m}
	ctx := requestContext(t, m, "", nil)
	user, err := h.CurrentUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Get maps absence to a zero Session, so Has must distinguish anonymity.
	if user != nil {
		t.Errorf("user = %+v, want nil for an anonymous visitor", user)
	}
}

func TestCurrentUserSignedIn(t *testing.T) {
	m := manager(t, session.NewMemoryStore())
	h := &ViewHelpers{Sessions: m}
	ctx := requestContext(t, m, "", func(ctx context.Context) {
		if err := m.Save(ctx, Session{Name: "ada"}); err != nil {
			t.Error(err)
		}
	})
	user, err := h.CurrentUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if user == nil || user.Name != "ada" {
		t.Fatalf("user = %+v, want the saved session", user)
	}
}

// Store failures must not render an existing session as signed out.
func TestCurrentUserStoreFailure(t *testing.T) {
	m := manager(t, failingStore{})
	h := &ViewHelpers{Sessions: m}
	ctx := requestContext(t, m, "an-existing-session", nil)
	if _, err := h.CurrentUser(ctx); err == nil {
		t.Fatal("want the store failure, got a signed-out page")
	}
}

func TestFlashesWithoutMiddleware(t *testing.T) {
	m := manager(t, session.NewMemoryStore())
	f, err := flash.New(m)
	if err != nil {
		t.Fatal(err)
	}
	h := &ViewHelpers{Sessions: m, Flash: f}
	msgs, err := h.Flashes(context.Background())
	if err != nil {
		t.Fatalf("a requestless render must not fail: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("messages = %v, want none outside a request", msgs)
	}
}

// A rendered flash is consumed.
func TestFlashesTakeOnce(t *testing.T) {
	m := manager(t, session.NewMemoryStore())
	f, err := flash.New(m)
	if err != nil {
		t.Fatal(err)
	}
	h := &ViewHelpers{Sessions: m, Flash: f}
	var first, second []flash.Message
	requestContext(t, m, "", func(ctx context.Context) {
		if err := f.Add(ctx, "success", "saved"); err != nil {
			t.Error(err)
			return
		}
		var err error
		if first, err = h.Flashes(ctx); err != nil {
			t.Error(err)
			return
		}
		if second, err = h.Flashes(ctx); err != nil {
			t.Error(err)
		}
	})
	if len(first) != 1 || first[0].Text != "saved" {
		t.Fatalf("first evaluation = %v, want the pending message", first)
	}
	if len(second) != 0 {
		t.Errorf("second evaluation = %v, want none: taking consumes", second)
	}
}

// An undecodable record exercises Get failure after Has succeeds.
func TestCurrentUserDecodeFailure(t *testing.T) {
	m := manager(t, corruptStore{})
	h := &ViewHelpers{Sessions: m}
	ctx := requestContext(t, m, "an-existing-session", nil)
	if _, err := h.CurrentUser(ctx); err == nil {
		t.Fatal("want the decode failure, got a signed-out page")
	}
}

func TestFlashesStoreFailure(t *testing.T) {
	m := manager(t, failingStore{})
	f, err := flash.New(m)
	if err != nil {
		t.Fatal(err)
	}
	h := &ViewHelpers{Sessions: m, Flash: f}
	ctx := requestContext(t, m, "an-existing-session", nil)
	if _, err := h.Flashes(ctx); err == nil {
		t.Fatal("want the store failure, got an empty flash region")
	}
}

type failingStore struct{}

func (failingStore) Load(context.Context, string) (session.Record, error) {
	return session.Record{}, errors.New("store unavailable")
}

func (failingStore) Save(context.Context, session.Record) (session.Record, error) {
	return session.Record{}, errors.New("store unavailable")
}

func (failingStore) Delete(context.Context, string) error { return errors.New("store unavailable") }

func TestActivePrefix(t *testing.T) {
	at := func(path string) context.Context {
		return web.WithRequest(context.Background(), httptest.NewRequest("GET", path, nil))
	}
	cases := []struct {
		ctx    context.Context
		prefix string
		want   string
	}{
		{at("/routes"), "/routes", "active"},
		{at("/routes/detail"), "/routes", "active"},
		{at("/greet"), "/routes", ""},
		// "/" prefixes every path, so home matches exactly or not at all.
		{at("/"), "/", "active"},
		{at("/greet"), "/", ""},
		{context.Background(), "/", ""},
	}
	for _, tc := range cases {
		if got := ActivePrefix(tc.ctx, tc.prefix); got != tc.want {
			t.Errorf("ActivePrefix(%q) = %q, want %q", tc.prefix, got, tc.want)
		}
	}
}

type corruptStore struct{}

func (corruptStore) Load(_ context.Context, sid string) (session.Record, error) {
	return session.Record{
		SID:            sid,
		Version:        1,
		AbsoluteExpiry: time.Now().Add(time.Hour),
		IdleExpiry:     time.Now().Add(time.Hour),
		Payload:        []byte(`{"app":"not-an-object"}`),
	}, nil
}

func (corruptStore) Save(_ context.Context, rec session.Record) (session.Record, error) {
	return rec, nil
}

func (corruptStore) Delete(context.Context, string) error { return nil }
