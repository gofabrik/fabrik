package forwardauth_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/gofabrik/fabrik/auth"
	"github.com/gofabrik/fabrik/auth/forwardauth"
)

// mustNew builds an Authenticator or fails the test.
func mustNew(t *testing.T, opts forwardauth.Options) *forwardauth.Authenticator {
	t.Helper()
	a, err := forwardauth.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestNew_RequiresUserHeader(t *testing.T) {
	if _, err := forwardauth.New(forwardauth.Options{}); err == nil {
		t.Error("empty UserHeader accepted")
	}
}

func TestAuthenticate_UserHeaderOnly(t *testing.T) {
	a := mustNew(t, forwardauth.Options{UserHeader: "X-Remote-User"})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Remote-User", "alice")

	id, err := a.Authenticate(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id.Subject != "alice" {
		t.Errorf("Subject = %q, want alice", id.Subject)
	}
	if id.Provider != "forward" {
		t.Errorf("Provider = %q, want forward", id.Provider)
	}
}

func TestAuthenticate_MissingHeader_Unauthenticated(t *testing.T) {
	a := mustNew(t, forwardauth.Options{UserHeader: "X-Remote-User"})

	_, err := a.Authenticate(httptest.NewRequest(http.MethodGet, "/", nil))
	if !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("err = %v, want ErrUnauthenticated", err)
	}
}

func TestAuthenticate_EmptyHeader_Unauthenticated(t *testing.T) {
	// Whitespace-only header value is treated as absent.
	a := mustNew(t, forwardauth.Options{UserHeader: "X-Remote-User"})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Remote-User", "   ")

	_, err := a.Authenticate(req)
	if !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("err = %v, want ErrUnauthenticated", err)
	}
}

func TestAuthenticate_AllAttributeHeaders(t *testing.T) {
	a := mustNew(t, forwardauth.Options{
		UserHeader:   "X-Remote-User",
		EmailHeader:  "X-Remote-Email",
		NameHeader:   "X-Remote-Name",
		GroupsHeader: "X-Remote-Groups",
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Remote-User", "alice")
	req.Header.Set("X-Remote-Email", "alice@example.com")
	req.Header.Set("X-Remote-Name", "Alice Anderson")
	req.Header.Set("X-Remote-Groups", "admins, users, ,  ops")

	id, err := a.Authenticate(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if id.Subject != "alice" {
		t.Errorf("Subject = %q, want alice", id.Subject)
	}
	if id.Email != "alice@example.com" {
		t.Errorf("Email = %q", id.Email)
	}
	if id.Name != "Alice Anderson" {
		t.Errorf("Name = %q", id.Name)
	}

	wantGroups := []string{"admins", "users", "ops"} // empty entry skipped
	gotGroups, ok := id.Claims["groups"].([]string)
	if !ok {
		t.Fatalf("Claims[groups] = %v, want []string", id.Claims["groups"])
	}
	if !reflect.DeepEqual(gotGroups, wantGroups) {
		t.Errorf("groups = %v, want %v", gotGroups, wantGroups)
	}
}

func TestAuthenticate_EmptyGroups_NoClaim(t *testing.T) {
	// Header present but empty / whitespace → no "groups" claim.
	a := mustNew(t, forwardauth.Options{
		UserHeader:   "X-Remote-User",
		GroupsHeader: "X-Remote-Groups",
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Remote-User", "alice")
	req.Header.Set("X-Remote-Groups", " , , ")

	id, err := a.Authenticate(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := id.Claims["groups"]; ok {
		t.Errorf("Claims[groups] should be absent when no non-empty entries")
	}
}

func TestAuthenticate_ProviderNameOverride(t *testing.T) {
	a := mustNew(t, forwardauth.Options{
		UserHeader:   "X-Remote-User",
		ProviderName: "internal-zone",
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Remote-User", "alice")

	id, _ := a.Authenticate(req)
	if id.Provider != "internal-zone" {
		t.Errorf("Provider = %q, want internal-zone", id.Provider)
	}
}

// Integration sanity: forwardauth slots into auth.Chain.
func TestForwardAuth_InChain(t *testing.T) {
	a := mustNew(t, forwardauth.Options{UserHeader: "X-Remote-User"})
	chain := auth.Chain(a) // single-element chain still tests the wiring

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Remote-User", "alice")

	id, err := chain.Authenticate(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id.Subject != "alice" || id.Provider != "forward" {
		t.Errorf("identity = %+v, want subject=alice provider=forward", id)
	}
}

func TestNewRejectsBlankUserHeader(t *testing.T) {
	if _, err := forwardauth.New(forwardauth.Options{UserHeader: "   "}); err == nil {
		t.Error("whitespace-only UserHeader accepted")
	}
}

// A header name with surrounding whitespace is trimmed before
// storing, so it still matches inbound headers.
func TestNewTrimsHeaderNames(t *testing.T) {
	a := mustNew(t, forwardauth.Options{UserHeader: " X-Remote-User "})
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Remote-User", "alice")
	id, err := a.Authenticate(r)
	if err != nil {
		t.Fatalf("trimmed header did not match: %v", err)
	}
	if id.Subject != "alice" {
		t.Fatalf("subject = %q", id.Subject)
	}
}
