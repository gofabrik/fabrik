package authsession_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/auth"
	"github.com/gofabrik/fabrik/auth/session"
	"github.com/gofabrik/fabrik/session"
)

type appSession struct{ Cart int }

func newManager(t *testing.T) *session.Manager[appSession] {
	t.Helper()
	m, err := session.New[appSession](session.Config{
		Store:          session.NewMemoryStore(),
		Token:          session.Cookie{},
		AbsoluteExpiry: time.Hour,
		IdleExpiry:     30 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// run drives one request through the session middleware, giving the
// handler the bridge and the session manager. Returns the response.
func run(t *testing.T, m *session.Manager[appSession], sid string, h func(ctx context.Context)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/", nil)
	if sid != "" {
		req.AddCookie(&http.Cookie{Name: "sid", Value: sid})
	}
	rr := httptest.NewRecorder()
	m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h(r.Context())
	})).ServeHTTP(rr, req)
	return rr
}

func cookie(rr *httptest.ResponseRecorder) string {
	for _, c := range rr.Result().Cookies() {
		if c.Name == "sid" {
			return c.Value
		}
	}
	return ""
}

func TestNewRejectsNil(t *testing.T) {
	if _, err := authsession.New(nil); err == nil {
		t.Error("nil Sessions accepted")
	}
	var m *session.Manager[appSession] // typed nil
	if _, err := authsession.New(m); err == nil {
		t.Error("typed-nil Sessions accepted")
	}
}

func TestNewIdempotentOnOneManager(t *testing.T) {
	m := newManager(t)
	if _, err := authsession.New(m); err != nil {
		t.Fatal(err)
	}
	if _, err := authsession.New(m); err != nil {
		t.Fatalf("second New on the same manager: %v", err)
	}
}

func TestUserKeyValidation(t *testing.T) {
	cases := []struct {
		provider, subject string
		wantErr           bool
	}{
		{"password", "u1", false},
		{"password", "sub:with:colons", false}, // subjects may contain ":"
		{"", "u1", true},
		{"password", "", true},
		{"pro:vider", "u1", true},
		{"session", "u1", true}, // reserved
	}
	for _, c := range cases {
		_, err := authsession.UserKey(c.provider, c.subject)
		if (err != nil) != c.wantErr {
			t.Errorf("UserKey(%q,%q) err=%v, wantErr=%v", c.provider, c.subject, err, c.wantErr)
		}
	}
	if got, _ := authsession.UserKey("password", "u1"); got != "password:u1" {
		t.Errorf("composed key = %q", got)
	}
}

func TestLoginRejectsBadIdentity(t *testing.T) {
	m := newManager(t)
	sa, _ := authsession.New(m)
	for _, id := range []auth.Identity{
		{Subject: "u1"},                        // no provider
		{Provider: "password"},                 // no subject
		{Provider: "session", Subject: "u1"},   // reserved
		{Provider: "pro:vider", Subject: "u1"}, // colon
	} {
		run(t, m, "", func(ctx context.Context) {
			if err := sa.Login(ctx, id); err == nil {
				t.Errorf("Login accepted bad identity %+v", id)
			}
		})
	}
}

func TestLoginThenAuthenticate(t *testing.T) {
	m := newManager(t)
	sa, _ := authsession.New(m)

	rr := run(t, m, "", func(ctx context.Context) {
		err := sa.Login(ctx, auth.Identity{
			Subject:  "u1",
			Email:    "alice@example.com",
			Name:     "Alice",
			Provider: "password",
			Claims:   map[string]any{"role": "admin"},
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	sid := cookie(rr)
	if sid == "" {
		t.Fatal("login did not establish a session")
	}

	// The session's UserID is the canonical key.
	uid, _ := m.Load(context.Background(), sid)
	_ = uid
	run(t, m, sid, func(ctx context.Context) {
		id, err := sa.Authenticate(reqOf(ctx))
		if err != nil {
			t.Fatal(err)
		}
		if id.Subject != "u1" || id.Provider != "password" || id.Email != "alice@example.com" {
			t.Fatalf("identity = %+v", id)
		}
		if id.Claims["role"] != "admin" {
			t.Fatalf("claims = %+v", id.Claims)
		}
	})
}

func TestAuthenticateAnonymous(t *testing.T) {
	m := newManager(t)
	sa, _ := authsession.New(m)
	run(t, m, "", func(ctx context.Context) {
		if _, err := sa.Authenticate(reqOf(ctx)); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Fatalf("anonymous Authenticate = %v, want ErrUnauthenticated", err)
		}
	})
}

func TestAuthenticateNoMiddlewareIs500Path(t *testing.T) {
	m := newManager(t)
	sa, _ := authsession.New(m)
	// No middleware: a bare context has no session state.
	req := httptest.NewRequest("GET", "/", nil)
	if _, err := sa.Authenticate(req); !errors.Is(err, session.ErrNoSession) {
		t.Fatalf("no-middleware Authenticate = %v, want ErrNoSession (fails closed)", err)
	}
	// Through a chain, a non-ErrUnauthenticated error short-circuits.
	chain := auth.Chain(sa)
	if _, err := chain.Authenticate(req); !errors.Is(err, session.ErrNoSession) {
		t.Fatalf("chain propagation = %v", err)
	}
}

func TestAppLevelPromoteReadsSubjectOnly(t *testing.T) {
	m := newManager(t)
	sa, _ := authsession.New(m)

	// App promotes directly, no auth library involved: UserID has no
	// ":" the bridge composed.
	rr := run(t, m, "", func(ctx context.Context) {
		if err := m.Promote(ctx, "raw-app-uid"); err != nil {
			t.Fatal(err)
		}
	})
	sid := cookie(rr)
	run(t, m, sid, func(ctx context.Context) {
		id, err := sa.Authenticate(reqOf(ctx))
		if err != nil {
			t.Fatal(err)
		}
		if id.Subject != "raw-app-uid" || id.Provider != "session" {
			t.Fatalf("app-promoted identity = %+v, want subject-only session provenance", id)
		}
	})
}

func TestLogoutIdempotentAndReal(t *testing.T) {
	m := newManager(t)
	sa, _ := authsession.New(m)

	// Idempotent on a sessionless visitor.
	run(t, m, "", func(ctx context.Context) {
		if err := sa.Logout(ctx); err != nil {
			t.Errorf("sessionless logout = %v", err)
		}
	})

	// Real logout ends the session.
	rr := run(t, m, "", func(ctx context.Context) {
		_ = sa.Login(ctx, auth.Identity{Subject: "u1", Provider: "password"})
	})
	sid := cookie(rr)
	run(t, m, sid, func(ctx context.Context) {
		if err := sa.Logout(ctx); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := m.Load(context.Background(), sid); err == nil {
		t.Fatal("logout left the session")
	}
}

// A cross-backend re-login overwrites the one cell: two writers, one
// identity, the latest wins - never a hybrid.
func TestCrossBackendReloginLatestWins(t *testing.T) {
	m := newManager(t)
	sa, _ := authsession.New(m)

	rr := run(t, m, "", func(ctx context.Context) {
		_ = sa.Login(ctx, auth.Identity{Subject: "A", Provider: "password", Email: "a@x"})
	})
	sid := cookie(rr)

	// Re-login as a different backend/user on the same session.
	rr = run(t, m, sid, func(ctx context.Context) {
		if _, err := sa.Authenticate(reqOf(ctx)); err != nil {
			t.Fatal(err)
		}
		_ = sa.Login(ctx, auth.Identity{Subject: "B", Provider: "forward", Email: "b@x"})
	})
	sid2 := cookie(rr)

	run(t, m, sid2, func(ctx context.Context) {
		id, err := sa.Authenticate(reqOf(ctx))
		if err != nil {
			t.Fatal(err)
		}
		if id.Subject != "B" || id.Provider != "forward" || id.Email != "b@x" {
			t.Fatalf("re-login identity = %+v, want the latest (B/forward)", id)
		}
	})
}

// reqOf builds a request carrying the session context, so
// Authenticate (which takes *http.Request) can be called from the
// context-shaped test handler.
func reqOf(ctx context.Context) *http.Request {
	return httptest.NewRequest("GET", "/", nil).WithContext(ctx)
}

// An app-owned UserID containing ":" (a tenant-scoped id) with no
// auth cell must read Subject-only, not be misparsed as an
// auth-composed key and fail closed.
func TestAppPromoteWithColonInUserID(t *testing.T) {
	m := newManager(t)
	sa, _ := authsession.New(m)

	rr := run(t, m, "", func(ctx context.Context) {
		if err := m.Promote(ctx, "tenant:user"); err != nil {
			t.Fatal(err)
		}
	})
	sid := cookie(rr)
	run(t, m, sid, func(ctx context.Context) {
		id, err := sa.Authenticate(reqOf(ctx))
		if err != nil {
			t.Fatalf("app-promoted colon id = %v, want subject-only identity", err)
		}
		if id.Subject != "tenant:user" || id.Provider != "session" {
			t.Fatalf("identity = %+v, want {tenant:user, session}", id)
		}
	})
}
