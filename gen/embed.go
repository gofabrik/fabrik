package gen

import (
	"strconv"
	"strings"
)

// EmbedCovers scans the annotation's comment group for //go:embed
// lines. found reports whether any exists; covered reports whether
// one of their patterns equals pattern. Directives anchored to
// embed.FS variables use it to require the all: form, wording their
// own diagnostics.
func EmbedCovers(a Annotation, pattern string) (found, covered bool) {
	for _, line := range a.Doc {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "//go:embed ")
		if !ok {
			continue
		}
		found = true
		for _, p := range embedPatterns(rest) {
			if p == pattern {
				return true, true
			}
		}
	}
	return found, false
}

// embedPatterns splits a go:embed argument list, including quoted patterns.
func embedPatterns(rest string) []string {
	var out []string
	for i := 0; i < len(rest); {
		switch rest[i] {
		case ' ', '\t':
			i++
		case '"', '`':
			quote := rest[i]
			end := strings.IndexByte(rest[i+1:], quote)
			if end < 0 {
				return out
			}
			raw := rest[i : i+end+2]
			if p, err := strconv.Unquote(raw); err == nil {
				out = append(out, p)
			}
			i += end + 2
		default:
			end := strings.IndexAny(rest[i:], " \t")
			if end < 0 {
				out = append(out, rest[i:])
				return out
			}
			out = append(out, rest[i:i+end])
			i += end
		}
	}
	return out
}
