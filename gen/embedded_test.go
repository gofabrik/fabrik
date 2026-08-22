package gen

import "testing"

func TestAssignOp(t *testing.T) {
	tests := []struct {
		name        string
		lhs         []string
		cleanupVar  string
		errDeclared bool
		want        string
	}{
		{"consumed value output", []string{"_", "sharedReport", "cleanup1"}, "cleanup1", true, ":="},
		{"all blank, err declared", []string{"_", "_"}, "", true, "="},
		{"all blank, err new", []string{"_", "_"}, "", false, ":="},
		{"cleanup only, err new", []string{"_", "cleanup1"}, "cleanup1", false, ":="},
		{"cleanup only, err declared", []string{"_", "cleanup1"}, "cleanup1", true, "="},
		{"cleanup without blanks, err declared", []string{"cleanup1"}, "cleanup1", true, "="},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := assignOp(tt.lhs, tt.cleanupVar, tt.errDeclared); got != tt.want {
				t.Fatalf("assignOp(%v, %q, %v) = %q, want %q", tt.lhs, tt.cleanupVar, tt.errDeclared, got, tt.want)
			}
		})
	}
}
