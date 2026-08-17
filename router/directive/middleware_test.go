package directive

import (
	"go/token"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/gen"
)

func parseMW(t *testing.T, args string) (*mwNode, []string) {
	t.Helper()
	m := NewMiddleware()
	n, ds := m.Parse(gen.Annotation{Pos: token.Position{Filename: "f.go", Line: 1, Column: 1}, Args: args})
	var msgs []string
	for _, d := range ds {
		msgs = append(msgs, d.Message)
	}
	nd, _ := n.(*mwNode)
	return nd, msgs
}

func TestParseOrderingOptions(t *testing.T) {
	nd, msgs := parseMW(t, "name=sessionauth global=true requires=session after=logger,metrics before=*")
	if len(msgs) != 0 {
		t.Fatalf("valid declaration: %v", msgs)
	}
	if !nd.global || nd.name != "sessionauth" || !nd.beforeAll || nd.afterAll {
		t.Fatalf("parsed node = %+v", nd)
	}
	if len(nd.requires) != 1 || nd.requires[0].name != "session" {
		t.Fatalf("requires = %+v", nd.requires)
	}
	if len(nd.after) != 2 || nd.after[0].name != "logger" || nd.after[1].name != "metrics" {
		t.Fatalf("after = %+v", nd.after)
	}
}

func TestParseGlobalValues(t *testing.T) {
	if nd, msgs := parseMW(t, "global=false name=x"); len(msgs) != 0 || nd.global {
		t.Fatalf("global=false: %v %+v", msgs, nd)
	}
	_, msgs := parseMW(t, "global=yes")
	if len(msgs) == 0 || !strings.Contains(msgs[0], "invalid global value") {
		t.Fatalf("global=yes should be a parse error, got %v", msgs)
	}
}

func TestParseUnknownOptionRejected(t *testing.T) {
	_, msgs := parseMW(t, "position=first")
	if len(msgs) == 0 {
		t.Fatal("an unknown option should be a parse error")
	}
}

func TestParseBandContradiction(t *testing.T) {
	_, msgs := parseMW(t, "global=true before=* after=*")
	found := false
	for _, msg := range msgs {
		if strings.Contains(msg, "contradict") {
			found = true
		}
	}
	if !found {
		t.Fatalf("before=* after=* should error, got %v", msgs)
	}
}

func TestParseOrderingRequiresGlobal(t *testing.T) {
	_, msgs := parseMW(t, "name=x after=session")
	found := false
	for _, msg := range msgs {
		if strings.Contains(msg, "not global") {
			found = true
		}
	}
	if !found {
		t.Fatalf("after= on a non-global should error, got %v", msgs)
	}
	// requires= is legal on route middleware; it validates per chain.
	if _, msgs := parseMW(t, "name=admin requires=authenticated"); len(msgs) != 0 {
		t.Fatalf("requires= on a named route middleware: %v", msgs)
	}
}

func TestParseRequiresStarRejected(t *testing.T) {
	_, msgs := parseMW(t, "global=true requires=*")
	if len(msgs) == 0 || !strings.Contains(msgs[0], "requires=*") {
		t.Fatalf("requires=* should be a parse error, got %v", msgs)
	}
}
