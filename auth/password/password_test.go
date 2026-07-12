package password_test

import (
	"context"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gofabrik/fabrik/auth"
	"github.com/gofabrik/fabrik/auth/password"
)

// memStore is the reference Store: case-folding, in-memory.
type memStore struct {
	mu    sync.Mutex
	users map[string]password.User // key: lowercased email
	nextN int
}

func newStore() *memStore { return &memStore{users: map[string]password.User{}} }

func (s *memStore) LookupByEmail(_ context.Context, email string) (password.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[strings.ToLower(email)]
	if !ok {
		return password.User{}, password.ErrUserNotFound
	}
	return u, nil
}

func (s *memStore) Create(_ context.Context, email string, hash []byte) (password.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := strings.ToLower(email)
	if _, ok := s.users[k]; ok {
		return password.User{}, password.ErrEmailTaken
	}
	s.nextN++
	u := password.User{ID: itoa(s.nextN), Email: k, PassHash: hash}
	s.users[k] = u
	return u, nil
}

func itoa(n int) string { return string(rune('0' + n)) }

// recSink records the last identity Login/Logout saw.
type recSink struct {
	loggedIn  *auth.Identity
	loggedOut bool
	loginErr  error
}

func (s *recSink) Login(_ context.Context, id auth.Identity) error {
	if s.loginErr != nil {
		return s.loginErr
	}
	s.loggedIn = &id
	return nil
}
func (s *recSink) Logout(_ context.Context) error { s.loggedOut = true; return nil }

func seed(t *testing.T, s *memStore, email, pass string) {
	t.Helper()
	h, err := testHasher.Hash(pass)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(context.Background(), email, h); err != nil {
		t.Fatal(err)
	}
}

// testHasher keeps bcrypt cost low so the suite is fast; one
// default-cost smoke test covers the real cost.
var testHasher = password.BcryptHasher{Cost: 4}

// newProv injects the cheap hasher unless the test set one.
func newProv(store password.Store, sink password.Sink, opts password.Options) (*password.Provider, error) {
	if opts.Hasher == nil {
		opts.Hasher = testHasher
	}
	return password.New(store, sink, opts)
}

func postJSON(body string) *http.Request {
	r := httptest.NewRequest("POST", "/", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestNewNilDeps(t *testing.T) {
	if _, err := password.New(nil, &recSink{}, password.Options{}); err == nil {
		t.Error("nil Store accepted")
	}
	if _, err := password.New(newStore(), nil, password.Options{}); err == nil {
		t.Error("nil Sink accepted")
	}
	var s *memStore // typed nil
	if _, err := password.New(s, &recSink{}, password.Options{}); err == nil {
		t.Error("typed-nil Store accepted")
	}
}

func TestLoginSuccess(t *testing.T) {
	store := newStore()
	seed(t, store, "alice@example.com", "password123")
	sink := &recSink{}
	ph, err := newProv(store, sink, password.Options{})
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	ph.Login(rr, postJSON(`{"email":"alice@example.com","password":"password123"}`))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("login = %d, want 204", rr.Code)
	}
	if sink.loggedIn == nil || sink.loggedIn.Provider != password.ProviderName || sink.loggedIn.Email != "alice@example.com" {
		t.Fatalf("sink identity = %+v", sink.loggedIn)
	}
	if sink.loggedIn.Subject == "" {
		t.Fatal("emitted identity has empty Subject")
	}
}

func TestLoginWrongPasswordNoSink(t *testing.T) {
	store := newStore()
	seed(t, store, "alice@example.com", "password123")
	sink := &recSink{}
	ph, _ := newProv(store, sink, password.Options{})

	rr := httptest.NewRecorder()
	ph.Login(rr, postJSON(`{"email":"alice@example.com","password":"wrong"}`))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d, want 401", rr.Code)
	}
	if sink.loggedIn != nil {
		t.Fatal("sink logged in on wrong password")
	}
}

func TestLoginUnknownEmailIndistinguishable(t *testing.T) {
	store := newStore()
	seed(t, store, "alice@example.com", "password123")
	ph, _ := newProv(store, &recSink{}, password.Options{})

	rr := httptest.NewRecorder()
	ph.Login(rr, postJSON(`{"email":"nobody@example.com","password":"whatever"}`))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unknown email = %d, want 401 (non-enumerating)", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "invalid credentials") {
		t.Fatalf("body leaks cause: %q", rr.Body.String())
	}
}

func TestLoginSessionErrorIs500(t *testing.T) {
	store := newStore()
	seed(t, store, "alice@example.com", "password123")
	sink := &recSink{loginErr: errors.New("store down")}
	ph, _ := newProv(store, sink, password.Options{})

	rr := httptest.NewRecorder()
	ph.Login(rr, postJSON(`{"email":"alice@example.com","password":"password123"}`))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("sink error = %d, want 500", rr.Code)
	}
}

func TestRegisterAutoLogin(t *testing.T) {
	store := newStore()
	sink := &recSink{}
	ph, _ := newProv(store, sink, password.Options{})

	rr := httptest.NewRecorder()
	ph.Register(rr, postJSON(`{"email":"new@example.com","password":"longenough"}`))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("register = %d, want 204", rr.Code)
	}
	if sink.loggedIn == nil || sink.loggedIn.Email != "new@example.com" {
		t.Fatalf("auto-login identity = %+v", sink.loggedIn)
	}
}

func TestRegisterTooShort400WithTypedError(t *testing.T) {
	ph, _ := newProv(newStore(), &recSink{}, password.Options{MinPasswordLength: 10})

	var gotErr error
	ph2, _ := newProv(newStore(), &recSink{}, password.Options{
		MinPasswordLength: 10,
		OnFailure: func(w http.ResponseWriter, r *http.Request, err error) {
			gotErr = err
			password.DefaultOnFailure(w, r, err)
		},
	})
	_ = ph

	rr := httptest.NewRecorder()
	ph2.Register(rr, postJSON(`{"email":"x@example.com","password":"short"}`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("too short = %d, want 400", rr.Code)
	}
	var pse *password.PasswordTooShortError
	if !errors.As(gotErr, &pse) || pse.Min != 10 {
		t.Fatalf("typed error = %v (Min via errors.As expected)", gotErr)
	}
	if strings.Contains(pse.Error(), "10") {
		t.Fatalf("Error() leaks the minimum: %q", pse.Error())
	}
}

func TestRegisterEmailTaken401(t *testing.T) {
	store := newStore()
	seed(t, store, "taken@example.com", "password123")
	ph, _ := newProv(store, &recSink{}, password.Options{})

	rr := httptest.NewRecorder()
	ph.Register(rr, postJSON(`{"email":"taken@example.com","password":"longenough"}`))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("email taken = %d, want 401 (non-enumerating)", rr.Code)
	}
}

func TestLogout(t *testing.T) {
	sink := &recSink{}
	ph, _ := newProv(newStore(), sink, password.Options{})
	rr := httptest.NewRecorder()
	ph.Logout(rr, httptest.NewRequest("POST", "/", nil))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("logout = %d, want 204", rr.Code)
	}
	if !sink.loggedOut {
		t.Fatal("sink did not see logout")
	}
}

// The typed Error Op values and the default routing matrix.
func TestErrorOpRouting(t *testing.T) {
	cases := []struct {
		err  error
		code int
	}{
		{&password.Error{Op: password.OpSession, Err: errors.New("x")}, http.StatusInternalServerError},
		{&password.Error{Op: password.OpLogout, Err: errors.New("x")}, http.StatusInternalServerError},
		{&password.Error{Op: password.OpHook, Err: errors.New("x")}, http.StatusInternalServerError},
		{&password.Error{Op: password.OpLookup, Err: errors.New("x")}, http.StatusInternalServerError},
		{&password.Error{Op: password.OpHash, Err: errors.New("x")}, http.StatusInternalServerError},
		{&password.Error{Op: password.OpCreate, Err: errors.New("x")}, http.StatusInternalServerError},
		{password.ErrEmailTaken, http.StatusUnauthorized}, // bare sentinel: the real register-taken path
		{&password.PasswordTooShortError{Min: 8}, http.StatusBadRequest},
		{password.ErrInvalidCredentials, http.StatusUnauthorized},
	}
	for _, c := range cases {
		rr := httptest.NewRecorder()
		password.DefaultOnFailure(rr, httptest.NewRequest("POST", "/", nil), c.err)
		if rr.Code != c.code {
			t.Errorf("route(%v) = %d, want %d", c.err, rr.Code, c.code)
		}
	}
}

// A store returning an empty ID is a contract violation, not a login.
func TestEmptyStoreIDIsError(t *testing.T) {
	store := &badIDStore{}
	sink := &recSink{}
	ph, _ := newProv(store, sink, password.Options{})

	rr := httptest.NewRecorder()
	ph.Login(rr, postJSON(`{"email":"x@example.com","password":"whatever12"}`))
	if sink.loggedIn != nil {
		t.Fatal("empty-ID user was logged in")
	}
	if rr.Code == http.StatusNoContent {
		t.Fatal("empty-ID login succeeded")
	}
}

// badIDStore returns a user with an empty ID from lookup.
type badIDStore struct{}

func (badIDStore) LookupByEmail(context.Context, string) (password.User, error) {
	h, _ := password.BcryptHasher{Cost: password.DefaultBcryptCost}.Hash("whatever12")
	return password.User{ID: "", Email: "x@example.com", PassHash: h}, nil
}
func (badIDStore) Create(context.Context, string, []byte) (password.User, error) {
	return password.User{}, errors.New("unused")
}

func TestMultipartCredentials(t *testing.T) {
	store := newStore()
	seed(t, store, "alice@example.com", "password123")
	ph, _ := newProv(store, &recSink{}, password.Options{})

	var buf strings.Builder
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("email", "alice@example.com")
	_ = mw.WriteField("password", "password123")
	mw.Close()

	r := httptest.NewRequest("POST", "/", strings.NewReader(buf.String()))
	r.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	ph.Login(rr, r)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("multipart login = %d, want 204 (multipart body must parse)", rr.Code)
	}
}

// A nil Hasher interface defaults to bcrypt; a typed-nil concrete
// hasher hides behind a non-nil interface and must be a boot error,
// not a construction-time panic.
type nilHasher struct{}

func (*nilHasher) Hash(string) ([]byte, error) { return nil, nil }
func (*nilHasher) Verify(string, []byte) bool  { return false }

func TestNewTypedNilHasher(t *testing.T) {
	var nh *nilHasher // typed nil, non-nil as a Hasher interface
	if _, err := password.New(newStore(), &recSink{}, password.Options{Hasher: nh}); err == nil {
		t.Error("typed-nil Hasher accepted")
	}
	// A genuinely nil interface defaults cleanly.
	var h password.Hasher
	if _, err := password.New(newStore(), &recSink{}, password.Options{Hasher: h}); err != nil {
		t.Errorf("nil Hasher interface should default: %v", err)
	}
}

// One smoke test at the real default cost, so the fast suite does
// not hide a cost-12 regression.
func TestLoginDefaultCostSmoke(t *testing.T) {
	store := newStore()
	h, _ := password.BcryptHasher{Cost: password.DefaultBcryptCost}.Hash("password123")
	store.mu.Lock()
	store.nextN++
	store.users["alice@example.com"] = password.User{ID: itoa(store.nextN), Email: "alice@example.com", PassHash: h}
	store.mu.Unlock()

	ph, err := password.New(store, &recSink{}, password.Options{}) // default bcrypt cost 12
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	ph.Login(rr, postJSON(`{"email":"alice@example.com","password":"password123"}`))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("default-cost login = %d, want 204", rr.Code)
	}
}

// A fallible OnSuccess routes its error through OnFailure (an
// OpHook 500) instead of the hook writing its own response.
func TestOnSuccessErrorRoutedTo500(t *testing.T) {
	store := newStore()
	seed(t, store, "alice@example.com", "password123")
	ph, _ := newProv(store, &recSink{}, password.Options{
		OnSuccess: func(w http.ResponseWriter, r *http.Request, id auth.Identity) error {
			return errors.New("flash store down")
		},
	})
	rr := httptest.NewRecorder()
	ph.Login(rr, postJSON(`{"email":"alice@example.com","password":"password123"}`))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("OnSuccess error = %d, want 500", rr.Code)
	}
}

// An OnSuccess follow-up failure classifies as OpHook, distinct from
// a session-backend failure.
func TestOnSuccessErrorIsOpHook(t *testing.T) {
	store := newStore()
	seed(t, store, "alice@example.com", "password123")
	var got error
	ph, _ := newProv(store, &recSink{}, password.Options{
		OnSuccess: func(w http.ResponseWriter, r *http.Request, id auth.Identity) error {
			return errors.New("flash down")
		},
		OnFailure: func(w http.ResponseWriter, r *http.Request, err error) {
			got = err
			password.DefaultOnFailure(w, r, err)
		},
	})
	ph.Login(httptest.NewRecorder(), postJSON(`{"email":"alice@example.com","password":"password123"}`))
	var e *password.Error
	if !errors.As(got, &e) || e.Op != password.OpHook {
		t.Fatalf("OnSuccess error op = %v, want OpHook", got)
	}
}

// A failed default auto-login (sink error) classifies as OpSession,
// not OpHook - it is a session failure, not a follow-up.
func TestAutoLoginSinkErrorIsOpSession(t *testing.T) {
	store := newStore()
	var got error
	ph, _ := newProv(store, &recSink{loginErr: errors.New("store down")}, password.Options{
		OnFailure: func(w http.ResponseWriter, r *http.Request, err error) {
			got = err
			password.DefaultOnFailure(w, r, err)
		},
	})
	ph.Register(httptest.NewRecorder(), postJSON(`{"email":"new@example.com","password":"longenough"}`))
	var e *password.Error
	if !errors.As(got, &e) || e.Op != password.OpSession {
		t.Fatalf("auto-login sink error op = %v, want OpSession", got)
	}
}

// A custom OnRegistered can return an error, routed as OpHook.
func TestOnRegisteredError(t *testing.T) {
	store := newStore()
	var got error
	ph, _ := newProv(store, &recSink{}, password.Options{
		OnRegistered: func(w http.ResponseWriter, r *http.Request, id auth.Identity) error {
			return errors.New("welcome email failed")
		},
		OnFailure: func(w http.ResponseWriter, r *http.Request, err error) {
			got = err
			password.DefaultOnFailure(w, r, err)
		},
	})
	ph.Register(httptest.NewRecorder(), postJSON(`{"email":"new@example.com","password":"longenough"}`))
	var e *password.Error
	if !errors.As(got, &e) || e.Op != password.OpHook {
		t.Fatalf("OnRegistered error op = %v, want OpHook", got)
	}
}

func TestJSONTrailingGarbageRejected(t *testing.T) {
	store := newStore()
	seed(t, store, "alice@example.com", "password123")
	ph, _ := newProv(store, &recSink{}, password.Options{})
	rr := httptest.NewRecorder()
	// Valid object followed by garbage.
	ph.Login(rr, postJSON(`{"email":"alice@example.com","password":"password123"} trailing`))
	if rr.Code == http.StatusNoContent {
		t.Fatal("login accepted a body with trailing data after the JSON object")
	}
}

// outageStore fails LookupByEmail and Create with a raw error.
type outageStore struct{ err error }

func (o outageStore) LookupByEmail(context.Context, string) (password.User, error) {
	return password.User{}, o.err
}
func (o outageStore) Create(context.Context, string, []byte) (password.User, error) {
	return password.User{}, o.err
}

func TestLoginLookupOutageIs500(t *testing.T) {
	ph, _ := newProv(outageStore{err: errors.New("db down")}, &recSink{}, password.Options{})
	rr := httptest.NewRecorder()
	ph.Login(rr, postJSON(`{"email":"a@example.com","password":"whatever12"}`))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("lookup outage = %d, want 500", rr.Code)
	}
}

func TestRegisterCreateOutageIs500(t *testing.T) {
	ph, _ := newProv(outageStore{err: errors.New("db down")}, &recSink{}, password.Options{})
	rr := httptest.NewRecorder()
	ph.Register(rr, postJSON(`{"email":"a@example.com","password":"longenough"}`))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("create outage = %d, want 500", rr.Code)
	}
}

func TestRegisterHashFailureIs500(t *testing.T) {
	ph, _ := newProv(newStore(), &recSink{}, password.Options{Hasher: failHasher{}})
	rr := httptest.NewRecorder()
	ph.Register(rr, postJSON(`{"email":"a@example.com","password":"longenough"}`))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("hash failure = %d, want 500", rr.Code)
	}
}

// failHasher succeeds on the boot dummy (empty input) but fails on
// real registration input.
type failHasher struct{}

func (failHasher) Hash(plain string) ([]byte, error) {
	if plain == "" {
		return []byte("dummy"), nil
	}
	return nil, errors.New("hash backend down")
}
func (failHasher) Verify(string, []byte) bool { return false }
