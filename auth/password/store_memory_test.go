package password

import (
	"context"
	"errors"
	"testing"

	"github.com/gofabrik/fabrik/auth"
)

func TestMemoryStoreNormalization(t *testing.T) {
	s := NewMemoryStore()
	s.Put("  Alice@Example.COM ", Credential{Hash: "h", Claims: auth.ClaimSet{Subject: "alice"}})
	variants := []string{"alice@example.com", "ALICE@EXAMPLE.COM", " alice@example.com\t"}
	for _, email := range variants {
		if _, err := s.Lookup(context.Background(), email); err != nil {
			t.Errorf("Lookup(%q) = %v", email, err)
		}
	}
	if err := s.UpdateHash(context.Background(), "ALICE@example.com", "h", "h2"); err != nil {
		t.Fatalf("UpdateHash via variant spelling: %v", err)
	}
	s.Delete("alice@EXAMPLE.com ")
	if _, err := s.Lookup(context.Background(), "alice@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete via variant spelling = %v, want ErrNotFound", err)
	}
}

func TestMemoryStoreLookupUnknown(t *testing.T) {
	s := NewMemoryStore()
	if _, err := s.Lookup(context.Background(), "nobody@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown = %v, want ErrNotFound", err)
	}
}

func TestMemoryStoreUpdateHashCAS(t *testing.T) {
	s := NewMemoryStore()
	s.Put("a@example.com", Credential{Hash: "h1"})
	if err := s.UpdateHash(context.Background(), "a@example.com", "h1", "h2"); err != nil {
		t.Fatalf("matching swap = %v", err)
	}
	cred, _ := s.Lookup(context.Background(), "a@example.com")
	if cred.Hash != "h2" {
		t.Fatalf("hash = %q after swap", cred.Hash)
	}
	if err := s.UpdateHash(context.Background(), "a@example.com", "h1", "h3"); !errors.Is(err, ErrHashChanged) {
		t.Fatalf("stale swap = %v, want ErrHashChanged", err)
	}
	cred, _ = s.Lookup(context.Background(), "a@example.com")
	if cred.Hash != "h2" {
		t.Fatal("stale swap replaced the hash")
	}
	if err := s.UpdateHash(context.Background(), "missing@example.com", "h1", "h2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing account swap = %v, want ErrNotFound", err)
	}
}

func TestMemoryStoreClonesAtBothSeams(t *testing.T) {
	s := NewMemoryStore()
	in := Credential{Hash: "h", Claims: auth.ClaimSet{Subject: "a", Roles: []string{"admin"}}}
	s.Put("a@example.com", in)
	in.Claims.Roles[0] = "evil-after-put"
	got, err := s.Lookup(context.Background(), "a@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got.Claims.Roles[0] != "admin" {
		t.Fatal("Put did not clone: caller mutation reached the store")
	}
	got.Claims.Roles[0] = "evil-after-lookup"
	again, _ := s.Lookup(context.Background(), "a@example.com")
	if again.Claims.Roles[0] != "admin" {
		t.Fatal("Lookup did not clone: reader mutation reached the store")
	}
}
