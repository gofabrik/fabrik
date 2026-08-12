package session

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/auth"
	fabriksession "github.com/gofabrik/fabrik/session"
)

func newManager(t *testing.T) *fabriksession.Manager[struct{}] {
	t.Helper()
	m, err := fabriksession.New[struct{}](fabriksession.Config{
		Store:          fabriksession.NewMemoryStore(),
		Token:          fabriksession.Cookie{Name: "sid"},
		AbsoluteExpiry: time.Hour,
		IdleExpiry:     time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestAuthenticateRestoresIdentity(t *testing.T) {
	m := newManager(t)
	method, err := New(m)
	if err != nil {
		t.Fatal(err)
	}

	login := httptest.NewRecorder()
	m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := method.Establish(r.Context(), "user-1", "local"); err != nil {
			t.Fatal(err)
		}
	})).ServeHTTP(login, httptest.NewRequest("GET", "/login", nil))
	cookies := login.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("login response set no session cookie")
	}

	var id *auth.Identity
	var authErr error
	r := httptest.NewRequest("GET", "/", nil)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, authErr = method.Authenticate(r)
	})).ServeHTTP(httptest.NewRecorder(), r)

	if authErr != nil {
		t.Fatal(authErr)
	}
	if id == nil || id.Subject != "user-1" || id.Issuer != "local" {
		t.Fatalf("Authenticate = %+v, want user-1/local", id)
	}
}

func TestAuthenticateAbstainsWithoutLogin(t *testing.T) {
	m := newManager(t)
	method, err := New(m)
	if err != nil {
		t.Fatal(err)
	}

	var id *auth.Identity
	var authErr error
	m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, authErr = method.Authenticate(r)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))

	if id != nil || authErr != nil {
		t.Fatalf("Authenticate = %+v, %v; want nil, nil", id, authErr)
	}
}

func TestAuthenticateErrorsOutsideSessionMiddleware(t *testing.T) {
	m := newManager(t)
	method, err := New(m)
	if err != nil {
		t.Fatal(err)
	}

	id, authErr := method.Authenticate(httptest.NewRequest("GET", "/", nil))
	if authErr == nil {
		t.Fatal("Authenticate outside the session middleware did not error")
	}
	if id != nil {
		t.Fatalf("Authenticate returned identity with error: %+v", id)
	}
}

func TestEstablishRejectsIncompleteKey(t *testing.T) {
	tests := []struct {
		name            string
		subject, issuer string
	}{
		{"empty subject", "", "local"},
		{"empty issuer", "user-1", ""},
		{"both empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newManager(t)
			method, err := New(m)
			if err != nil {
				t.Fatal(err)
			}

			var estErr error
			m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				estErr = method.Establish(r.Context(), tt.subject, tt.issuer)
			})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/login", nil))

			if estErr == nil {
				t.Fatalf("Establish(%q, %q) did not error", tt.subject, tt.issuer)
			}
		})
	}
}

func TestClearRemovesIdentity(t *testing.T) {
	m := newManager(t)
	method, err := New(m)
	if err != nil {
		t.Fatal(err)
	}

	login := httptest.NewRecorder()
	m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := method.Establish(r.Context(), "user-1", "local"); err != nil {
			t.Fatal(err)
		}
	})).ServeHTTP(login, httptest.NewRequest("GET", "/login", nil))

	logout := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/logout", nil)
	for _, c := range login.Result().Cookies() {
		r.AddCookie(c)
	}
	m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := method.Clear(r.Context()); err != nil {
			t.Fatal(err)
		}
	})).ServeHTTP(logout, r)

	var id *auth.Identity
	var authErr error
	after := httptest.NewRequest("GET", "/", nil)
	for _, c := range login.Result().Cookies() {
		after.AddCookie(c)
	}
	m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, authErr = method.Authenticate(r)
	})).ServeHTTP(httptest.NewRecorder(), after)

	if id != nil || authErr != nil {
		t.Fatalf("Authenticate after Clear = %+v, %v; want nil, nil", id, authErr)
	}
}

var _ auth.Authenticator = (*Method)(nil)
