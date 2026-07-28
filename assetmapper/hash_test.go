package assetmapper

import "testing"

func TestHashContentLength(t *testing.T) {
	got := hashContent([]byte("x"))
	if len(got) != HashLength {
		t.Fatalf("hash length = %d, want %d", len(got), HashLength)
	}
	if got != "2d711642b726b0440162" {
		t.Fatalf("hash = %q, want first %d SHA-256 hex characters", got, HashLength)
	}
}
