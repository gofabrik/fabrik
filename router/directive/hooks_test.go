package directive

import (
	"go/token"
	"testing"
)

// One handler per hook, whichever directive claims it first; methods are
// independent claims.
func TestClaimHook(t *testing.T) {
	h := &Host{}
	posA := token.Position{Filename: "a.go", Line: 1}
	posB := token.Position{Filename: "b.go", Line: 9}

	if _, ok := h.ClaimHook("NotFound", "http:notfound", posA); !ok {
		t.Fatal("first claim refused")
	}
	if first, ok := h.ClaimHook("NotFound", "http:notfound", posB); ok || first.Directive != "http:notfound" || first.Pos != posA {
		t.Fatalf("duplicate claim = %+v ok=%v, want refusal naming the first", first, ok)
	}
	if first, ok := h.ClaimHook("NotFound", "web:notfound", posB); ok || first.Directive != "http:notfound" {
		t.Fatalf("cross-directive claim = %+v ok=%v, want refusal naming the raw variant", first, ok)
	}
	if _, ok := h.ClaimHook("MethodNotAllowed", "web:methodnotallowed", posB); !ok {
		t.Fatal("an unclaimed hook was refused")
	}
	if first, ok := h.ClaimHook("MethodNotAllowed", "http:methodnotallowed", posA); ok || first.Directive != "web:methodnotallowed" {
		t.Fatalf("raw-after-web claim = %+v ok=%v, want refusal naming the web variant", first, ok)
	}
}
