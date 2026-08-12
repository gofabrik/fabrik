package auth

import (
	"reflect"
	"slices"
	"testing"
	"time"
)

func sample() *Identity {
	return &Identity{
		Subject:       "user-1",
		Issuer:        "local",
		Name:          "Alice",
		Email:         "alice@example.test",
		EmailVerified: true,
		Roles:         []string{"admin", "editor"},
		Scope:         "profile reports:read",
		AuthTime:      time.Date(2026, 8, 12, 9, 30, 0, 0, time.UTC),
		Extra:         map[string]any{"tenant": "north"},
	}
}

func TestIdentityIsAuthenticated(t *testing.T) {
	tests := []struct {
		name string
		id   *Identity
		want bool
	}{
		{"nil receiver", nil, false},
		{"empty subject", &Identity{Issuer: "local"}, false},
		{"subject set", &Identity{Subject: "user-1"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.id.IsAuthenticated(); got != tt.want {
				t.Fatalf("IsAuthenticated() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCloneLossless(t *testing.T) {
	id := sample()
	id.Extra["sub"] = "smuggled"
	id.Extra["exp"] = 12345

	c := id.Clone()
	if c == nil {
		t.Fatal("Clone() = nil")
	}
	if c.Subject != id.Subject || c.Issuer != id.Issuer || c.Name != id.Name ||
		c.Email != id.Email || c.EmailVerified != id.EmailVerified ||
		c.Scope != id.Scope || !c.AuthTime.Equal(id.AuthTime) {
		t.Fatalf("Clone() changed scalar fields: %+v != %+v", c, id)
	}
	if !slices.Equal(c.Roles, id.Roles) {
		t.Fatalf("Clone() Roles = %v, want %v", c.Roles, id.Roles)
	}
	wantExtra := map[string]any{"tenant": "north", "sub": "smuggled", "exp": 12345}
	if !reflect.DeepEqual(c.Extra, wantExtra) {
		t.Fatalf("Clone() Extra = %v, want %v", c.Extra, wantExtra)
	}
}

func TestCloneNil(t *testing.T) {
	var id *Identity
	if c := id.Clone(); c != nil {
		t.Fatalf("nil.Clone() = %+v, want nil", c)
	}
}

func TestCloneDoesNotAlias(t *testing.T) {
	id := sample()
	c := id.Clone()

	c.Roles[0] = "changed"
	c.Extra["tenant"] = "changed"

	if id.Roles[0] != "admin" {
		t.Fatalf("mutating clone Roles changed original: %v", id.Roles)
	}
	if id.Extra["tenant"] != "north" {
		t.Fatalf("mutating clone Extra changed original: %v", id.Extra)
	}
}
