package auth

import (
	"testing"
)

func TestClaimsTransport(t *testing.T) {
	ctx := t.Context()

	if c, ok := Claims(ctx); ok || c != nil {
		t.Fatalf("empty context carries claims: %v", c)
	}

	if got := WithClaims(ctx, nil); got != ctx {
		t.Fatal("nil claims changed the context")
	}

	first := &ClaimSet{Subject: "alice"}
	ctx1 := WithClaims(ctx, first)
	if c, ok := Claims(ctx1); !ok || c != first {
		t.Fatalf("Claims = %v, %v; want the attached set", c, ok)
	}

	second := &ClaimSet{Subject: "bob"}
	ctx2 := WithClaims(ctx1, second)
	if c, _ := Claims(ctx2); c != second {
		t.Fatalf("override lost: got %v", c)
	}
	if c, _ := Claims(ctx1); c != first {
		t.Fatal("outer context mutated by override")
	}
}

func TestPredicates(t *testing.T) {
	ctx := t.Context()
	tests := []struct {
		name          string
		claims        *ClaimSet
		authenticated bool
	}{
		{"no claims", nil, false},
		{"subjectless", &ClaimSet{Scope: "read"}, false},
		{"subject", &ClaimSet{Subject: "alice"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := WithClaims(ctx, tt.claims)
			if got := IsAuthenticated(c); got != tt.authenticated {
				t.Errorf("IsAuthenticated = %v, want %v", got, tt.authenticated)
			}
		})
	}

	full := WithClaims(ctx, &ClaimSet{
		Subject: "alice",
		Scope:   "read write",
		Roles:   []string{"admin", "editor"},
	})
	if !HasScope(full, "write") || HasScope(full, "delete") {
		t.Error("HasScope wrong on present/absent scopes")
	}
	if !HasRole(full, "admin") || HasRole(full, "viewer") {
		t.Error("HasRole wrong on present/absent roles")
	}
	if HasScope(ctx, "read") || HasRole(ctx, "admin") {
		t.Error("predicates true on empty context")
	}
	// Subjectless client claims can still carry grants.
	subjectless := WithClaims(ctx, &ClaimSet{Scope: "read", Roles: []string{"admin"}})
	if !HasScope(subjectless, "read") || !HasRole(subjectless, "admin") {
		t.Error("grants not read from subjectless claims")
	}
}
