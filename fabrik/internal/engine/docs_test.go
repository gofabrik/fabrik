package engine

import (
	"os"
	"testing"
)

// TestDirectivesDoc verifies the generated directive reference.
func TestDirectivesDoc(t *testing.T) {
	want, err := os.ReadFile("../../../DIRECTIVES.md")
	if err != nil {
		t.Fatalf("read DIRECTIVES.md: %v", err)
	}
	if got := DirectivesDoc(); got != string(want) {
		t.Errorf("DIRECTIVES.md is stale; regenerate with: fabrik directives > DIRECTIVES.md\n--- got ---\n%s", got)
	}
}
