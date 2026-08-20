package session

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/auth"
	"github.com/gofabrik/fabrik/session"
)

var (
	_ Manager            = (*session.Manager)(nil)
	_ auth.Authenticator = (*Auth)(nil)
)

func newManager(t *testing.T, store session.Store, logger *slog.Logger) *session.Manager {
	t.Helper()
	if store == nil {
		store = session.NewMemoryStore(session.MemoryOptions{})
	}
	m, err := session.New(session.Config{
		Store:          store,
		Token:          session.Cookie{},
		AbsoluteExpiry: time.Hour,
		Logger:         logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func newAuth(t *testing.T, m Manager, opts Options) *Auth {
	t.Helper()
	a, err := New(m, opts)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func serve(t *testing.T, m *session.Manager, sid string, h http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if sid != "" {
		req.AddCookie(&http.Cookie{Name: "sid", Value: sid}) //nolint:gosec // test-only cookie, security attributes irrelevant
	}
	rr := httptest.NewRecorder()
	m.Middleware(h).ServeHTTP(rr, req)
	return rr
}

func sidFrom(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	for _, c := range rr.Result().Cookies() {
		if c.Name == "sid" && c.Value != "" {
			return c.Value
		}
	}
	return ""
}

func TestLoginAuthenticateRoundTrip(t *testing.T) {
	m := newManager(t, nil, nil)
	a := newAuth(t, m, Options{})

	claims := &auth.ClaimSet{
		Subject:      "alice",
		Issuer:       "test",
		Audience:     auth.Audience{"web", "api"},
		IssuedAt:     auth.NewNumericDate(time.Unix(1700000000, 0)),
		ID:           "jti-1",
		Scope:        "read write",
		ClientID:     "client-1",
		Roles:        []string{"admin"},
		Groups:       []string{"staff"},
		Entitlements: []string{"billing"},
	}
	rr := serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		if err := a.Login(r.Context(), claims); err != nil {
			t.Fatal(err)
		}
	})
	sid := sidFrom(t, rr)
	if sid == "" {
		t.Fatal("login minted no session")
	}

	serve(t, m, sid, func(w http.ResponseWriter, r *http.Request) {
		c, err := a.Authenticate(r)
		if err != nil || c == nil {
			t.Fatalf("Authenticate = %v, %v", c, err)
		}
		if !reflect.DeepEqual(c, claims) {
			t.Fatalf("claims changed through the cell:\n in=%+v\nout=%+v", claims, c)
		}
	})
}

func TestLoginStripsValidityWindow(t *testing.T) {
	m := newManager(t, nil, nil)
	a := newAuth(t, m, Options{})

	claims := &auth.ClaimSet{
		Subject:   "alice",
		Expiry:    auth.NewNumericDate(time.Now().Add(time.Minute)),
		NotBefore: auth.NewNumericDate(time.Now()),
		IssuedAt:  auth.NewNumericDate(time.Unix(1700000000, 0)),
	}
	rr := serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		if err := a.Login(r.Context(), claims); err != nil {
			t.Fatal(err)
		}
	})
	serve(t, m, sidFrom(t, rr), func(w http.ResponseWriter, r *http.Request) {
		c, err := a.Authenticate(r)
		if err != nil || c == nil {
			t.Fatalf("Authenticate = %v, %v", c, err)
		}
		if c.Expiry != nil || c.NotBefore != nil {
			t.Fatalf("validity window survived the snapshot: exp=%v nbf=%v", c.Expiry, c.NotBefore)
		}
		if c.IssuedAt == nil {
			t.Fatal("iat stripped")
		}
	})
	if claims.Expiry == nil || claims.NotBefore == nil {
		t.Fatal("Login mutated the caller's claims")
	}
}

func TestLoginRotatesSID(t *testing.T) {
	m := newManager(t, nil, nil)
	a := newAuth(t, m, Options{})

	rr := serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		if err := m.Save(r.Context(), struct{ N int }{1}); err != nil {
			t.Fatal(err)
		}
	})
	pre := sidFrom(t, rr)
	if pre == "" {
		t.Fatal("no pre-login session")
	}

	rr = serve(t, m, pre, func(w http.ResponseWriter, r *http.Request) {
		if err := a.Login(r.Context(), &auth.ClaimSet{Subject: "alice"}); err != nil {
			t.Fatal(err)
		}
	})
	post := sidFrom(t, rr)
	if post == "" || post == pre {
		t.Fatalf("login did not rotate: pre=%q post=%q", pre, post)
	}
}

func TestLoginRequiresSubject(t *testing.T) {
	m := newManager(t, nil, nil)
	a := newAuth(t, m, Options{})
	serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		if err := a.Login(r.Context(), &auth.ClaimSet{}); err == nil {
			t.Error("subjectless login accepted")
		}
		if err := a.Login(r.Context(), nil); err == nil {
			t.Error("nil claims accepted")
		}
	})
}

func TestLogoutDestroysSession(t *testing.T) {
	mem := session.NewMemoryStore(session.MemoryOptions{})
	m := newManager(t, mem, nil)
	a := newAuth(t, m, Options{})

	rr := serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		if err := a.Login(r.Context(), &auth.ClaimSet{Subject: "alice"}); err != nil {
			t.Fatal(err)
		}
	})
	sid := sidFrom(t, rr)

	serve(t, m, sid, func(w http.ResponseWriter, r *http.Request) {
		if err := a.Logout(r.Context()); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := mem.Load(context.Background(), sid); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("record survived logout: %v", err)
	}
}

func TestLogoutWithoutSession(t *testing.T) {
	m := newManager(t, nil, nil)
	a := newAuth(t, m, Options{})

	if err := a.Logout(context.Background()); err != nil {
		t.Fatalf("logout without middleware = %v", err)
	}
	serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		if err := a.Logout(r.Context()); err != nil {
			t.Fatalf("logout without session = %v", err)
		}
	})
}

func TestAuthenticateTriState(t *testing.T) {
	m := newManager(t, nil, nil)
	a := newAuth(t, m, Options{})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if c, err := a.Authenticate(req); c != nil || err != nil {
		t.Fatalf("no middleware: got %v, %v; want nil, nil", c, err)
	}

	serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		if c, err := a.Authenticate(r); c != nil || err != nil {
			t.Fatalf("claim-less session: got %v, %v; want nil, nil", c, err)
		}
	})
}

type failingStore struct {
	session.Store
	fail bool
}

var errStore = errors.New("store down")

func (s *failingStore) Load(ctx context.Context, sid string) (session.Record, error) {
	if s.fail {
		return session.Record{}, errStore
	}
	return s.Store.Load(ctx, sid)
}

func TestReadFailureStaysAnError(t *testing.T) {
	fs := &failingStore{Store: session.NewMemoryStore(session.MemoryOptions{})}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	m := newManager(t, fs, logger)
	a := newAuth(t, m, Options{Logger: logger})

	rr := serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		if err := a.Login(r.Context(), &auth.ClaimSet{Subject: "alice"}); err != nil {
			t.Fatal(err)
		}
	})
	sid := sidFrom(t, rr)

	fs.fail = true
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "sid", Value: sid}) //nolint:gosec // test-only cookie, security attributes irrelevant
	rr = httptest.NewRecorder()
	m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Direct authentication preserves the store error.
		if c, err := a.Authenticate(r); c != nil || !errors.Is(err, errStore) {
			t.Fatalf("store failure: got %v, %v; want nil wrapping the store error", c, err)
		}
		// Middleware logs the error and continues anonymously.
		var reached bool
		a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r2 *http.Request) {
			reached = true
			if _, ok := auth.Claims(r2.Context()); ok {
				t.Error("claims attached despite store failure")
			}
		})).ServeHTTP(w, r)
		if !reached {
			t.Fatal("request dropped on store failure")
		}
	})).ServeHTTP(rr, req)

	if log := buf.String(); !strings.Contains(log, "session auth") || !strings.Contains(log, "store down") {
		t.Fatalf("read failure not logged with the underlying error: %q", log)
	}
}

func TestMiddlewareAttachesAndDefers(t *testing.T) {
	m := newManager(t, nil, nil)
	a := newAuth(t, m, Options{})

	rr := serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		if err := a.Login(r.Context(), &auth.ClaimSet{Subject: "alice"}); err != nil {
			t.Fatal(err)
		}
	})
	sid := sidFrom(t, rr)

	serve(t, m, sid, func(w http.ResponseWriter, r *http.Request) {
		a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r2 *http.Request) {
			c, ok := auth.Claims(r2.Context())
			if !ok || c.Subject != "alice" {
				t.Fatalf("middleware attached %v, %v", c, ok)
			}
		})).ServeHTTP(w, r)

		// Existing claims take precedence over session claims.
		prior := &auth.ClaimSet{Subject: "token-user"}
		r2 := r.WithContext(auth.WithClaims(r.Context(), prior))
		a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r3 *http.Request) {
			if c, _ := auth.Claims(r3.Context()); c != prior {
				t.Fatalf("session claims overrode existing claims: %v", c)
			}
		})).ServeHTTP(w, r2)
	})
}

func TestMiddlewareDefaultLoggerLogsReadFailure(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	fs := &failingStore{Store: session.NewMemoryStore(session.MemoryOptions{})}
	m := newManager(t, fs, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	a := newAuth(t, m, Options{})

	rr := serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		if err := a.Login(r.Context(), &auth.ClaimSet{Subject: "alice"}); err != nil {
			t.Fatal(err)
		}
	})
	sid := sidFrom(t, rr)
	fs.fail = true

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "sid", Value: sid}) //nolint:gosec // test-only cookie, security attributes irrelevant
	m.Middleware(a.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))).
		ServeHTTP(httptest.NewRecorder(), req)

	if !strings.Contains(buf.String(), "session auth") {
		t.Fatalf("default logger did not receive the read failure: %q", buf.String())
	}
}

type promoteFailManager struct {
	*session.Manager
}

var errPromote = errors.New("promote refused")

func (promoteFailManager) Promote(context.Context, string) error { return errPromote }

type destroyFailManager struct {
	*session.Manager
}

var errDestroy = errors.New("destroy refused")

func (destroyFailManager) Destroy(context.Context) error { return errDestroy }

func TestAuthenticateRejectsIdentityMismatch(t *testing.T) {
	m := newManager(t, nil, nil)
	a := newAuth(t, m, Options{})

	rr := serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		if err := a.Login(r.Context(), &auth.ClaimSet{Subject: "alice"}); err != nil {
			t.Fatal(err)
		}
	})
	sid := sidFrom(t, rr)

	// Changing the session user invalidates the stored claims.
	rr = serve(t, m, sid, func(w http.ResponseWriter, r *http.Request) {
		if err := m.Promote(r.Context(), "mallory"); err != nil {
			t.Fatal(err)
		}
	})
	serve(t, m, sidFrom(t, rr), func(w http.ResponseWriter, r *http.Request) {
		c, err := a.Authenticate(r)
		if c != nil || err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("identity mismatch: got %v, %v; want nil, mismatch error", c, err)
		}
	})
}

func TestLogoutSurfacesOtherErrors(t *testing.T) {
	m := newManager(t, nil, nil)
	a := newAuth(t, destroyFailManager{m}, Options{})
	serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		if err := a.Logout(r.Context()); !errors.Is(err, errDestroy) {
			t.Fatalf("Logout = %v, want the destroy error surfaced", err)
		}
	})
}

func TestLoginFailingPromoteStagesNothing(t *testing.T) {
	mem := session.NewMemoryStore(session.MemoryOptions{})
	m := newManager(t, mem, nil)
	a := newAuth(t, promoteFailManager{m}, Options{})

	rr := serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		if err := a.Login(r.Context(), &auth.ClaimSet{Subject: "alice"}); !errors.Is(err, errPromote) {
			t.Fatalf("Login = %v, want the promote error", err)
		}
	})
	if sid := sidFrom(t, rr); sid != "" {
		rec, err := mem.Load(context.Background(), sid)
		t.Fatalf("session committed after failed promote: %v %v", rec, err)
	}
}

type corruptStore struct {
	session.Store
	corrupt bool
}

func (s *corruptStore) Load(ctx context.Context, sid string) (session.Record, error) {
	rec, err := s.Store.Load(ctx, sid)
	if err == nil && s.corrupt {
		rec.Payload = []byte(`{broken`)
	}
	return rec, nil
}

func TestLoginSaveFailureDestroysSession(t *testing.T) {
	mem := session.NewMemoryStore(session.MemoryOptions{})
	cs := &corruptStore{Store: mem}
	m := newManager(t, cs, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	a := newAuth(t, m, Options{Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))})

	rr := serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		if err := m.Save(r.Context(), struct{ N int }{1}); err != nil {
			t.Fatal(err)
		}
	})
	sid := sidFrom(t, rr)
	cs.corrupt = true

	serve(t, m, sid, func(w http.ResponseWriter, r *http.Request) {
		err := a.Login(r.Context(), &auth.ClaimSet{Subject: "alice"})
		if err == nil {
			t.Fatal("login succeeded on a corrupt cell envelope")
		}
		if errors.Is(err, errPromote) {
			t.Fatal("wrong failure path")
		}
	})
	// A failed snapshot removes both the old and promoted records.
	if _, err := mem.Load(context.Background(), sid); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("record survived the failed login: %v", err)
	}
	if sids, err := mem.ListByUser(context.Background(), "alice"); err != nil || len(sids) != 0 {
		t.Fatalf("promoted record committed despite the failed login: %v %v", sids, err)
	}
}
