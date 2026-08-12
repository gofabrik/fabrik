package auth

import (
	"context"
	"reflect"
	"slices"
	"testing"
)

// reservedNames lists claims that must not survive in Extra.
var reservedNames = []string{
	"sub", "iss", "name", "email", "email_verified", "roles", "scope", "auth_time",
	"exp", "nbf", "iat", "jti", "aud", "azp", "nonce", "at_hash",
}

func TestFromAbsent(t *testing.T) {
	id, ok := From(context.Background())
	if ok || id != nil {
		t.Fatalf("From(empty ctx) = %v, %v; want nil, false", id, ok)
	}
	if IsAuthenticated(context.Background()) {
		t.Fatal("IsAuthenticated(empty ctx) = true")
	}
}

func TestNewContextRoundTrip(t *testing.T) {
	ctx := NewContext(context.Background(), sample())
	id, ok := From(ctx)
	if !ok {
		t.Fatal("From after NewContext: ok = false")
	}
	want := sample()
	if id.Subject != want.Subject || id.Issuer != want.Issuer ||
		id.Name != want.Name || id.Email != want.Email ||
		id.EmailVerified != want.EmailVerified || id.Scope != want.Scope ||
		!id.AuthTime.Equal(want.AuthTime) {
		t.Fatalf("round-trip changed fields: %+v", id)
	}
	if !slices.Equal(id.Roles, want.Roles) {
		t.Fatalf("round-trip Roles = %v, want %v", id.Roles, want.Roles)
	}
	if !reflect.DeepEqual(id.Extra, map[string]any{"tenant": "north"}) {
		t.Fatalf("round-trip Extra = %v, want only tenant", id.Extra)
	}
	if !IsAuthenticated(ctx) {
		t.Fatal("IsAuthenticated = false for attached subject")
	}
}

func TestNewContextNilClears(t *testing.T) {
	ctx := NewContext(context.Background(), sample())
	cleared := NewContext(ctx, nil)

	if id, ok := From(cleared); ok || id != nil {
		t.Fatalf("From(cleared) = %v, %v; want nil, false", id, ok)
	}
	if IsAuthenticated(cleared) {
		t.Fatal("IsAuthenticated(cleared) = true")
	}
	if _, ok := From(ctx); !ok {
		t.Fatal("clearing a derived context affected the parent")
	}
}

func TestNewContextDetachesFromCaller(t *testing.T) {
	src := sample()
	ctx := NewContext(context.Background(), src)

	src.Subject = "changed"
	src.Roles[0] = "changed"
	src.Extra["tenant"] = "changed"

	id, _ := From(ctx)
	if id.Subject != "user-1" || id.Roles[0] != "admin" || id.Extra["tenant"] != "north" {
		t.Fatalf("caller mutation reached the carried identity: %+v", id)
	}
}

func TestFromReturnsFreshCopies(t *testing.T) {
	ctx := NewContext(context.Background(), sample())

	first, _ := From(ctx)
	first.Subject = "changed"
	first.Roles[0] = "changed"
	first.Extra["tenant"] = "changed"

	second, _ := From(ctx)
	if second.Subject != "user-1" || second.Roles[0] != "admin" || second.Extra["tenant"] != "north" {
		t.Fatalf("mutating one From result affected the next: %+v", second)
	}
}

func TestNewContextStripsReserved(t *testing.T) {
	for _, name := range reservedNames {
		t.Run(name, func(t *testing.T) {
			id := sample()
			id.Extra[name] = "smuggled"
			got, _ := From(NewContext(context.Background(), id))
			if _, present := got.Extra[name]; present {
				t.Fatalf("reserved Extra[%q] survived attachment", name)
			}
			if got.Extra["tenant"] != "north" {
				t.Fatalf("unreserved key lost while stripping %q: %v", name, got.Extra)
			}
		})
	}
}

func TestIsAuthenticatedEmptySubjectCarrier(t *testing.T) {
	ctx := NewContext(context.Background(), &Identity{Issuer: "device", Extra: map[string]any{"device_id": "d1"}})

	if id, ok := From(ctx); !ok || id == nil {
		t.Fatalf("carrier without subject should be readable: %v, %v", id, ok)
	}
	if IsAuthenticated(ctx) {
		t.Fatal("IsAuthenticated = true for empty subject")
	}
}
