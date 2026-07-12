package web_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	fauth "github.com/gofabrik/fabrik/auth"
	"github.com/gofabrik/fabrik/auth/password"
	authweb "github.com/gofabrik/fabrik/auth/web"
)

// memStore + recSink: minimal Store/Sink for exercising the UI.
type memStore struct {
	mu    sync.Mutex
	users map[string]password.User
	n     int
}

func newStore() *memStore { return &memStore{users: map[string]password.User{}} }
func (s *memStore) LookupByEmail(_ context.Context, e string) (password.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[strings.ToLower(e)]
	if !ok {
		return password.User{}, password.ErrUserNotFound
	}
	return u, nil
}
func (s *memStore) Create(_ context.Context, e string, h []byte) (password.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := strings.ToLower(e)
	if _, ok := s.users[k]; ok {
		return password.User{}, password.ErrEmailTaken
	}
	s.n++
	u := password.User{ID: string(rune('a' + s.n)), Email: k, PassHash: h}
	s.users[k] = u
	return u, nil
}

type sink struct{ id *fauth.Identity }

func (s *sink) Login(_ context.Context, id fauth.Identity) error { s.id = &id; return nil }
func (s *sink) Logout(_ context.Context) error                   { s.id = nil; return nil }

func newUI(t *testing.T, opts authweb.Options) (http.Handler, *sink) {
	t.Helper()
	sk := &sink{}
	// cheap hasher via Options passthrough is not exposed, so seed
	// with the default and accept cost; tests are few.
	ui, err := authweb.New(newStore(), sk, opts)
	if err != nil {
		t.Fatal(err)
	}
	return ui.Handler(), sk
}

func TestLoginPageRenders(t *testing.T) {
	h, _ := newUI(t, authweb.Options{})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/auth/login", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "<form") {
		t.Fatalf("login page = %d:\n%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `action="/auth/login"`) {
		t.Fatalf("form action missing prefix:\n%s", rr.Body.String())
	}
}

func TestRegisterRedirectsAndLogsIn(t *testing.T) {
	h, sk := newUI(t, authweb.Options{})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, form("/auth/register", "new@example.com", "password123"))
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/auth/account" {
		t.Fatalf("register = %d, loc %q, want 303 -> /auth/account", rr.Code, rr.Header().Get("Location"))
	}
	if sk.id == nil {
		t.Fatal("register did not log in through the sink")
	}
}

func TestLoginFailureRerendersForm(t *testing.T) {
	h, _ := newUI(t, authweb.Options{})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, form("/auth/login", "nobody@example.com", "wrongpass"))
	if rr.Code != http.StatusOK {
		t.Fatalf("bad login = %d, want 200 re-render", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Invalid email or password.") {
		t.Fatalf("error not shown inline:\n%s", body)
	}
	if !strings.Contains(body, `value="nobody@example.com"`) {
		t.Fatalf("email not preserved on re-render:\n%s", body)
	}
}

func TestDisableRegister(t *testing.T) {
	h, _ := newUI(t, authweb.Options{DisableRegister: true})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/auth/register", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("disabled register = %d, want 404", rr.Code)
	}
}

func TestAccountProtectedByAuth(t *testing.T) {
	deny := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "no", http.StatusUnauthorized)
		})
	}
	h, _ := newUI(t, authweb.Options{Auth: deny})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/auth/account", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("account without auth = %d, want 401", rr.Code)
	}
	// No Auth option => no account route.
	h2, _ := newUI(t, authweb.Options{})
	rr = httptest.NewRecorder()
	h2.ServeHTTP(rr, httptest.NewRequest("GET", "/auth/account", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("account with nil Auth = %d, want 404", rr.Code)
	}
}

func TestCustomPrefixAndRedirect(t *testing.T) {
	h, _ := newUI(t, authweb.Options{Prefix: "/u", AfterLogin: "/home"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, form("/u/register", "x@example.com", "password123"))
	if rr.Header().Get("Location") != "/home" {
		t.Fatalf("custom AfterLogin = %q", rr.Header().Get("Location"))
	}
}

func form(path, email, pass string) *http.Request {
	body := url.Values{"email": {email}, "password": {pass}}.Encode()
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

// outageSink fails Login, so a login attempt reaches OpSession.
type outageSink struct{}

func (outageSink) Login(context.Context, fauth.Identity) error {
	return errors.New("session store down")
}
func (outageSink) Logout(context.Context) error { return errors.New("session store down") }

// An operational failure (session write down) is a 500, not a 200
// login form dressed as "invalid credentials".
func TestOperationalFailureIs500NotFormError(t *testing.T) {
	store := newStore()
	// Seed a user so the credentials verify and the flow reaches the
	// sink, where the outage happens.
	h, _ := password.BcryptHasher{Cost: 4}.Hash("password123")
	store.users["alice@example.com"] = password.User{ID: "1", Email: "alice@example.com", PassHash: h}
	ui, err := authweb.New(store, outageSink{}, authweb.Options{
		MinPasswordLength: 1,
		// keep bcrypt cheap-ish via the seeded default; login only verifies
	})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	ui.Handler().ServeHTTP(rr, form("/auth/login", "alice@example.com", "password123"))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("operational failure = %d, want 500 (not a 200 credential form):\n%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "Invalid email or password") {
		t.Fatalf("operational failure lied with a credential message:\n%s", rr.Body.String())
	}
}
