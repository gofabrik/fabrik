package directive

import (
	"testing"
	"time"
)

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
		if got := mustHelp(c.doc); got != c.want {
			t.Errorf("mustHelp(%q) = %q, want %q", c.doc, got, c.want)
		}
	}
}

func mustHelp(text string) string {
	help, _ := helpAndLong(text)
	return help
}

func TestHelpAndLong(t *testing.T) {
	cases := []struct {
		doc, help, long string
	}{
		{"Start the server.\n", "Start the server", ""},
		{"Summary sentence. Additional detail.\n", "Summary sentence", "Additional detail."},
		{"Start the HTTP\nserver.\n\nWarms caches before listening.\n", "Start the HTTP server", "Warms caches before listening."},
		{"Apply migrations.\n\nfabrik:cli:command\n", "Apply migrations", ""},
		{"Apply migrations.\n\nFirst detail paragraph.\n\nSecond paragraph.\n", "Apply migrations", "First detail paragraph.\n\nSecond paragraph."},
		{"Use [Store] values. Additional detail.\n", "Use [Store] values", "Additional detail."},
		{"Talk to A. Smith. More detail.\n", "Talk to A. Smith", "More detail."},
		{"fooA. More detail.\n", "fooA. More detail", ""},
		{"\u958b\u59cb\u3002\u8a73\u7d30\u3067\u3059\u3002\n", "\u958b\u59cb\u3002", "\u8a73\u7d30\u3067\u3059\u3002"},
	}
	for _, c := range cases {
		help, long := helpAndLong(c.doc)
		if help != c.help || long != c.long {
			t.Errorf("helpAndLong(%q) = (%q, %q), want (%q, %q)", c.doc, help, long, c.help, c.long)
		}
	}
}

func TestDurationLiteral(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"90s", "1*time.Minute + 30*time.Second"},
		{"0s", "0"},
		{"-90m", "-(1*time.Hour + 30*time.Minute)"},
		{"-2562047h47m16.854775808s", "-9223372036854775808 * time.Nanosecond"},
	}
	for _, c := range cases {
		d, err := time.ParseDuration(c.in)
		if err != nil {
			t.Fatalf("ParseDuration(%q): %v", c.in, err)
		}
		if got := durationLiteral(d, "time"); got != c.want {
			t.Errorf("durationLiteral(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
