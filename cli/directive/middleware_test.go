package directive

import (
	"go/token"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

func parseMW(t *testing.T, args string) (*mwNode, []string) {
	t.Helper()
	m := &Middleware{fam: newFamily()}
	n, ds := m.Parse(gen.Annotation{Pos: token.Position{Filename: "f.go", Line: 1, Column: 1}, Args: args})
	var msgs []string
	for _, d := range ds {
		msgs = append(msgs, d.Message)
	}
	nd, _ := n.(*mwNode)
	return nd, msgs
}

func TestParseRequires(t *testing.T) {
	nd, msgs := parseMW(t, "name=retry requires=confirm,audit-log")
	if len(msgs) != 0 {
		t.Fatalf("valid declaration: %v", msgs)
	}
	if len(nd.requires) != 2 || nd.requires[0].name != "confirm" || nd.requires[1].name != "audit-log" {
		t.Fatalf("requires = %+v", nd.requires)
	}
	// Reference positions are relative to the annotation's argument column.
	wantCol := len("name=retry requires=")
	if nd.requires[0].pos.Column != wantCol || nd.requires[1].pos.Column != wantCol+len("confirm,") {
		t.Fatalf("ref columns = %d, %d; want %d, %d",
			nd.requires[0].pos.Column, nd.requires[1].pos.Column, wantCol, wantCol+len("confirm,"))
	}

	if _, msgs := parseMW(t, "name=retry requires=Confirm"); len(msgs) == 0 || !strings.Contains(msgs[0], "invalid CLI token") {
		t.Fatalf("non-kebab requires target accepted: %v", msgs)
	}
	if _, msgs := parseMW(t, "name=retry requires=*"); len(msgs) == 0 {
		t.Fatal("requires=* accepted")
	}
}

func mwDecl(fam *family, name string, requires ...string) *mwNode {
	nd := &mwNode{pos: token.Position{Filename: "m.go", Line: len(fam.mwOrder) + 1}, name: name}
	for i, r := range requires {
		nd.requires = append(nd.requires, mwRef{name: r, pos: token.Position{Filename: "m.go", Line: nd.pos.Line, Column: 10 + i}})
	}
	fam.middlewares[name] = nd
	fam.mwOrder = append(fam.mwOrder, nd)
	return nd
}

func TestEarlierNames(t *testing.T) {
	fam := newFamily()
	fam.root = &rootNode{middleware: []string{"confirm"}}
	fam.groups = append(fam.groups,
		&groupNode{path: []string{"database"}, middleware: []string{"retry"}},
		&groupNode{path: []string{"database", "backup"}, middleware: []string{"slow"}},
		&groupNode{path: []string{"server"}, middleware: []string{"other"}},
	)
	got := fam.earlierNames([]string{"database", "backup", "run"})
	if strings.Join(got, ",") != "confirm,retry,slow" {
		t.Fatalf("earlierNames = %v", got)
	}
	// Executable ancestor commands contribute middleware.
	fam.commands = append(fam.commands, cmdReg{path: []string{"database", "backup"}, middleware: []string{"deep"}})
	got = fam.earlierNames([]string{"database", "backup", "run"})
	if strings.Join(got, ",") != "confirm,retry,slow,deep" &&
		strings.Join(got, ",") != "confirm,retry,deep,slow" {
		t.Fatalf("ancestor command middleware missing: %v", got)
	}
	if got := fam.earlierNames([]string{"database"}); strings.Join(got, ",") != "confirm" {
		t.Fatalf("group's own chain must not include itself: %v", got)
	}
}

func TestValidateRequires(t *testing.T) {
	fam := newFamily()
	mwDecl(fam, "confirm")
	mwDecl(fam, "retry", "confirm")
	mwDecl(fam, "audit", "retry")

	pos := token.Position{Filename: "cmd.go", Line: 5}
	if ds := fam.validateRequires(pos, []string{"confirm", "retry"}, nil); len(ds) != 0 {
		t.Fatalf("satisfied by earlier entry: %v", ds)
	}
	if ds := fam.validateRequires(pos, []string{"retry"}, []string{"confirm"}); len(ds) != 0 {
		t.Fatalf("satisfied by ancestor: %v", ds)
	}
	ds := fam.validateRequires(pos, []string{"retry"}, nil)
	if len(ds) != 1 || ds[0].Severity != diag.SevError || !strings.Contains(ds[0].Message, "does not run earlier") {
		t.Fatalf("unsatisfied requires: %v", ds)
	}
	if ds[0].Pos != pos {
		t.Fatalf("chain error position = %v, want the chain site %v", ds[0].Pos, pos)
	}
	// Chain validation defers unknown targets to declaration validation.
	mwDecl(fam, "ghosted", "phantom")
	if ds := fam.validateRequires(pos, []string{"ghosted"}, nil); len(ds) != 0 {
		t.Fatalf("unknown target double-reported at chain: %v", ds)
	}
}

func TestRequiresDeclarationChecks(t *testing.T) {
	fam := newFamily()
	mwDecl(fam, "confirm")
	mwDecl(fam, "lost", "phantom")
	ds := fam.checkRequiresDecls()
	if len(ds) != 1 || !strings.Contains(ds[0].Message, "unknown CLI middleware") {
		t.Fatalf("declaration checks = %v, want one unknown-target error", ds)
	}
	if ds[0].Pos.Column != 10 {
		t.Fatalf("unknown-target diagnostic at %v, want the requires ref position", ds[0].Pos)
	}
	// Unreferenced cycles warn but do not block generation.
	fam2 := newFamily()
	mwDecl(fam2, "a", "b")
	mwDecl(fam2, "b", "a")
	if ds := fam2.checkRequiresDecls(); len(ds) != 0 {
		t.Fatalf("unused cycle blocked generation: %v", ds)
	}
}
