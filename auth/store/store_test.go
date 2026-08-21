package store_test

import (
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/auth/password"
	"github.com/gofabrik/fabrik/auth/store"
)

// The domain rules shared by every backend have one authoritative
// definition in the root; these pin it.

func TestValidIdentityID(t *testing.T) {
	if store.ValidIdentityID("") {
		t.Fatal("empty id accepted")
	}
	if !store.ValidIdentityID(strings.Repeat("a", store.MaxIdentityIDLen)) {
		t.Fatal("boundary-length id rejected")
	}
	if store.ValidIdentityID(strings.Repeat("a", store.MaxIdentityIDLen+1)) {
		t.Fatal("oversized id accepted")
	}
	if !store.ValidIdentityID("u\x00id") {
		t.Fatal("binary id rejected")
	}
}

func TestNormalizeEmail(t *testing.T) {
	norm, ok := store.NormalizeEmail("  Alice@Example.COM ")
	if !ok || norm != "alice@example.com" {
		t.Fatalf("NormalizeEmail = %q, %v", norm, ok)
	}
	if _, ok := store.NormalizeEmail("   "); ok {
		t.Fatal("blank email accepted")
	}
	if _, ok := store.NormalizeEmail(strings.Repeat("a", password.MaxEmailLen+1)); ok {
		t.Fatal("oversized email accepted")
	}
	domain := "@example.com"
	longest := strings.Repeat("a", password.MaxEmailLen-len(domain)) + domain
	if norm, ok := store.NormalizeEmail(" " + longest); !ok || norm != longest {
		t.Fatalf("boundary email after trim = %q, %v", norm, ok)
	}
}

func TestClaimsRoundTrip(t *testing.T) {
	claims := store.IdentityClaims{
		Roles:        []string{"admin", "editor"},
		Groups:       []string{"staff"},
		Entitlements: []string{"beta"},
	}
	text, err := store.MarshalClaims(claims)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.UnmarshalClaims(text)
	if err != nil {
		t.Fatal(err)
	}
	for name, pair := range map[string][2][]string{
		"roles":        {got.Roles, claims.Roles},
		"groups":       {got.Groups, claims.Groups},
		"entitlements": {got.Entitlements, claims.Entitlements},
	} {
		if len(pair[0]) != len(pair[1]) {
			t.Fatalf("%s did not round-trip: %v", name, pair[0])
		}
		for i := range pair[0] {
			if pair[0][i] != pair[1][i] {
				t.Fatalf("%s did not round-trip: %v", name, pair[0])
			}
		}
	}

	empty, err := store.MarshalClaims(store.IdentityClaims{})
	if err != nil {
		t.Fatal(err)
	}
	if empty != "{}" {
		t.Fatalf("empty claims = %q, want {}", empty)
	}
	if _, err := store.UnmarshalClaims("not json"); err == nil {
		t.Fatal("malformed column value accepted")
	}
}
