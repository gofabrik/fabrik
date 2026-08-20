package gen

import "testing"

func TestLowerFirst(t *testing.T) {
	for in, want := range map[string]string{
		"HumanizeAge": "humanizeAge",
		"MD":          "mD",
		"upper":       "upper",
		"":            "",
	} {
		if got := LowerFirst(in); got != want {
			t.Errorf("LowerFirst(%q) = %q, want %q", in, got, want)
		}
	}
}
