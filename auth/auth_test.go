package auth_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/auth"
)

// --- helpers ---

// stubAuth returns a fixed (Identity, error) on every call. Use to
// drive chain-order tests without spinning up real backends.
type stubAuth struct {
	id    auth.Identity
	err   error
	calls *int
}

func (s *stubAuth) Authenticate(r *http.Request) (auth.Identity, error) {
	if s.calls != nil {
		*s.calls++
	}
	return s.id, s.err
}

// --- Chain ---

func TestChain_FirstSuccessWins(t *testing.T) {
	first := &stubAuth{id: auth.Identity{Subject: "first"}}
	second := &stubAuth{id: auth.Identity{Subject: "second"}}

	chain := auth.Chain(first, second)
	id, err := chain.Authenticate(httptest.NewRequest(http.MethodGet, "/", nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id.Subject != "first" {
		t.Errorf("Subject = %q, want first", id.Subject)
	}
}

func TestChain_FallthroughOnUnauthenticated(t *testing.T) {
	first := &stubAuth{err: auth.ErrUnauthenticated}
	second := &stubAuth{id: auth.Identity{Subject: "second"}}

	chain := auth.Chain(first, second)
	id, err := chain.Authenticate(httptest.NewRequest(http.MethodGet, "/", nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id.Subject != "second" {
		t.Errorf("Subject = %q, want second", id.Subject)
	}
}

func TestChain_NonSentinelErrorShortCircuits(t *testing.T) {
	// A store outage on the first authenticator must not silently
	// fall through to a less-trusted authenticator.
	outage := errors.New("redis: connection refused")
	secondCalls := 0
	first := &stubAuth{err: outage}
	second := &stubAuth{id: auth.Identity{Subject: "second"}, calls: &secondCalls}

	chain := auth.Chain(first, second)
	_, err := chain.Authenticate(httptest.NewRequest(http.MethodGet, "/", nil))
	if !errors.Is(err, outage) {
		t.Errorf("err = %v, want outage", err)
	}
	if secondCalls != 0 {
		t.Errorf("second was called %d times, want 0", secondCalls)
	}
}

func TestChain_EmptyReturnsUnauthenticated(t *testing.T) {
	_, err := auth.Chain().Authenticate(httptest.NewRequest(http.MethodGet, "/", nil))
	if !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("err = %v, want ErrUnauthenticated", err)
	}
}

func TestChain_AllFallthroughReturnsUnauthenticated(t *testing.T) {
	first := &stubAuth{err: auth.ErrUnauthenticated}
	second := &stubAuth{err: auth.ErrUnauthenticated}

	chain := auth.Chain(first, second)
	_, err := chain.Authenticate(httptest.NewRequest(http.MethodGet, "/", nil))
	if !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("err = %v, want ErrUnauthenticated", err)
	}
}

// TestChain_PanicsOnNilEntry pins the fail-loud-at-construction
// contract: a nil Authenticator must panic at Chain time, not later
// inside a request handler. The panic message must name the offending
// index so configuration bugs are diagnosable from the stack alone.
func TestChain_PanicsOnNilEntry(t *testing.T) {
	cases := []struct {
		name      string
		args      []auth.Authenticator
		wantIndex string
	}{
		{
			name:      "single nil",
			args:      []auth.Authenticator{nil},
			wantIndex: "index 0",
		},
		{
			name:      "nil first",
			args:      []auth.Authenticator{nil, &stubAuth{}},
			wantIndex: "index 0",
		},
		{
			name:      "nil middle",
			args:      []auth.Authenticator{&stubAuth{}, nil, &stubAuth{}},
			wantIndex: "index 1",
		},
		{
			name:      "nil last",
			args:      []auth.Authenticator{&stubAuth{}, &stubAuth{}, nil},
			wantIndex: "index 2",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				v := recover()
				if v == nil {
					t.Fatal("expected panic on nil Authenticator entry")
				}
				msg, _ := v.(string)
				if !strings.Contains(msg, "nil Authenticator") {
					t.Errorf("panic = %q, want message mentioning 'nil Authenticator'", msg)
				}
				if !strings.Contains(msg, tc.wantIndex) {
					t.Errorf("panic = %q, want it to name %q so the offending position is obvious", msg, tc.wantIndex)
				}
			}()
			_ = auth.Chain(tc.args...)
		})
	}
}

func TestAuthenticatorFunc_Adapter(t *testing.T) {
	want := auth.Identity{Subject: "u"}
	a := auth.AuthenticatorFunc(func(r *http.Request) (auth.Identity, error) {
		return want, nil
	})
	id, err := a.Authenticate(httptest.NewRequest(http.MethodGet, "/", nil))
	if err != nil || id.Subject != "u" {
		t.Errorf("got (%+v, %v), want (%+v, nil)", id, err, want)
	}
}

// --- Required / Optional middleware ---

func TestRequired_NilAuthenticatorPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic on nil Authenticator")
		}
	}()
	_ = auth.Required(nil)
}

func TestOptional_NilAuthenticatorPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic on nil Authenticator")
		}
	}()
	_ = auth.Optional(nil)
}

func TestRequired_401OnMiss(t *testing.T) {
	a := &stubAuth{err: auth.ErrUnauthenticated}
	called := false
	h := auth.Required(a)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", rec.Code)
	}
	if called {
		t.Error("next handler ran despite 401")
	}
}

func TestRequired_StashesIdentity(t *testing.T) {
	want := auth.Identity{Subject: "u", Email: "u@example.com", Provider: "test"}
	a := &stubAuth{id: want}

	var got auth.Identity
	var ok bool
	h := auth.Required(a)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok = auth.FromContext(r.Context())
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if !ok {
		t.Fatal("FromContext returned ok=false")
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("identity = %+v, want %+v", got, want)
	}
}

func TestRequired_500OnStoreError(t *testing.T) {
	a := &stubAuth{err: errors.New("redis: down")}
	called := false
	h := auth.Required(a)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d, want 500", rec.Code)
	}
	if called {
		t.Error("next handler ran despite store error")
	}
}

func TestOptional_PassesThroughOnMiss(t *testing.T) {
	a := &stubAuth{err: auth.ErrUnauthenticated}
	called := false
	var fromCtx auth.Identity
	var ok bool
	h := auth.Optional(a)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		fromCtx, ok = auth.FromContext(r.Context())
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if !called {
		t.Error("next handler did not run")
	}
	if ok {
		t.Errorf("FromContext ok=true, want false; got %+v", fromCtx)
	}
}

func TestOptional_StashesWhenAuthenticated(t *testing.T) {
	want := auth.Identity{Subject: "u"}
	a := &stubAuth{id: want}

	var got auth.Identity
	var ok bool
	h := auth.Optional(a)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok = auth.FromContext(r.Context())
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if !ok {
		t.Fatal("FromContext ok=false")
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("identity = %+v, want %+v", got, want)
	}
}

func TestOptional_DegradesToAnonymousOnStoreError(t *testing.T) {
	a := &stubAuth{err: errors.New("redis: down")}
	called, gotID := false, false
	h := auth.Optional(a)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, gotID = auth.FromContext(r.Context())
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	// Optional grants no authorization, so a backend outage must not
	// take down a public route: the request continues anonymously and
	// no identity is stashed. Required still fails closed elsewhere.
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d, want 200 (optional degrades to anonymous)", rec.Code)
	}
	if !called {
		t.Error("next handler did not run despite optional auth")
	}
	if gotID {
		t.Error("an identity was stashed despite the auth error")
	}
}

func TestTypedNilAuthenticatorPanicsAtConstruction(t *testing.T) {
	var a *stubAuth // typed nil, non-nil interface
	for _, ctor := range []struct {
		name string
		fn   func()
	}{
		{"Required", func() { auth.Required(a) }},
		{"Optional", func() { auth.Optional(a) }},
		{"Chain", func() { auth.Chain(a) }},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s did not panic on a typed-nil authenticator", ctor.name)
				}
			}()
			ctor.fn()
		}()
	}
}

func TestTypedNilFuncAdapterPanicsAtConstruction(t *testing.T) {
	var f auth.AuthenticatorFunc // nil func, non-nil interface
	defer func() {
		if recover() == nil {
			t.Error("Required did not panic on a nil AuthenticatorFunc")
		}
	}()
	auth.Required(f)
}
