package directive

import (
	"go/token"
	"slices"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/gen"
)

func parseMW(t *testing.T, args string) (any, []string) {
	t.Helper()
	m := NewMiddleware()
	n, ds := m.Parse(gen.Annotation{Pos: token.Position{Filename: "f.go", Line: 1, Column: 1}, Args: args})
	var msgs []string
	for _, d := range ds {
		msgs = append(msgs, d.Message)
	}
	return n, msgs
}

func TestParseInsertValues(t *testing.T) {
	if _, msgs := parseMW(t, "insert=first"); len(msgs) != 0 {
		t.Fatalf("insert=first: %v", msgs)
	}
	if _, msgs := parseMW(t, "insert=last"); len(msgs) != 0 {
		t.Fatalf("insert=last: %v", msgs)
	}
	_, msgs := parseMW(t, "insert=middle")
	if len(msgs) == 0 || !strings.Contains(msgs[0], "insert") {
		t.Fatalf("insert=middle should be a parse error, got %v", msgs)
	}
}

func TestParseInsertWithNameRejected(t *testing.T) {
	const want = "insert orders global middleware; named middleware are ordered by their middleware= list"
	for _, args := range []string{"name=auth insert=last", "name= insert=last"} {
		_, msgs := parseMW(t, args)
		if !slices.Contains(msgs, want) {
			t.Fatalf("%q should carry exactly %q, got %v", args, want, msgs)
		}
	}
}
