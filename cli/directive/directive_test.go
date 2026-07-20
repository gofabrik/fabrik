package directive

import "testing"

func TestKebabCase(t *testing.T) {
	cases := map[string]string{
		"Serve":     "serve",
		"Work":      "work",
		"ServeHTTP": "serve-http",
		"DBReset":   "db-reset",
		"Migrate":   "migrate",
		"RunAll":    "run-all",
		"HTTPServe": "http-serve",
		"A":         "a",
	}
	for in, want := range cases {
		if got := kebabCase(in); got != want {
			t.Errorf("kebabCase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHelpFromDoc(t *testing.T) {
	cases := []struct {
		doc  string
		want string
	}{
		{"Start the HTTP server.\n\nfabrik:cli:command\n", "Start the HTTP server"},
		{"Apply pending database migrations. Extra detail follows here.\nfabrik:cli:command\n", "Apply pending database migrations"},
		{"Start the HTTP\nserver.\n\nLonger detail paragraph.\n\nfabrik:cli:command\n", "Start the HTTP server"},
		{"Talk to A. Smith. More detail.\nfabrik:cli:command\n", "Talk to A. Smith"},
		{"fabrik:cli:command\n", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := helpFromDoc(c.doc); got != c.want {
			t.Errorf("helpFromDoc(%q) = %q, want %q", c.doc, got, c.want)
		}
	}
}
