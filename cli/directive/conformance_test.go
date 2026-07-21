package directive

import (
	"go/token"
	"io"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/cli"
	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

// The generator and CLI library must reject the same invalid declarations.

func libRejects(t *testing.T, root *cli.Command, wantErr string) {
	t.Helper()
	var errb strings.Builder
	code := root.Exec([]string{root.Subcommands[0].Name}, cli.WithStderr(&errb), cli.WithStdout(io.Discard))
	if code == 0 || !strings.Contains(errb.String(), wantErr) {
		t.Errorf("library accepted a declaration the generator rejects: code=%d stderr=%q want %q", code, errb.String(), wantErr)
	}
}

func wireDiags(t *testing.T, dir gen.Directive, args string) diag.Diagnostics {
	t.Helper()
	_, ds := dir.Parse(gen.Annotation{Name: dir.Name(), Args: args, Pos: token.Position{Filename: "t.go", Line: 1, Column: 1}})
	return ds
}

func wantDiag(t *testing.T, ds diag.Diagnostics, fragment string) {
	t.Helper()
	for _, d := range ds {
		if strings.Contains(d.Message, fragment) {
			return
		}
	}
	t.Errorf("wire side missing diagnostic containing %q, got %v", fragment, ds)
}

func parseInput(t *testing.T, kind inputKind, args string) *inputNode {
	t.Helper()
	in := &Input{fam: newFamily(), kind: kind}
	n, ds := in.Parse(gen.Annotation{Name: in.Name(), Args: args, Pos: token.Position{Filename: "t.go", Line: 1, Column: 1}})
	if n == nil || ds.HasFatal() {
		t.Fatalf("input %q did not parse: %v", args, ds)
	}
	return n.(*inputNode)
}

func TestConformance_DuplicateShortFlags(t *testing.T) {
	fam := newFamily()
	fam.commands = append(fam.commands, cmdReg{path: []string{"migrate"}, fn: "Migrate"})
	fam.inputs[nil] = []*inputNode{
		parseInput(t, kindFlag, "name=dry-run short=n type=bool"),
		parseInput(t, kindFlag, "name=count short=n type=int default=1"),
	}
	wantDiag(t, fam.validateChains(), "share short -n")

	libRejects(t, &cli.Command{
		Name: "app",
		Subcommands: []*cli.Command{{
			Name:  "migrate",
			Flags: cli.Flags(cli.BoolFlag("dry-run").Short('n'), cli.IntFlag("count").Short('n')),
			Run:   func(cli.Context) error { return nil },
		}},
	}, "short")
}

func TestConformance_ReservedHelpFlag(t *testing.T) {
	flag := &Input{fam: newFamily(), kind: kindFlag}
	wantDiag(t, wireDiags(t, flag, "name=help type=bool"), "reserved by the cli library")

	libRejects(t, &cli.Command{
		Name: "app",
		Subcommands: []*cli.Command{{
			Name:  "migrate",
			Flags: cli.Flags(cli.BoolFlag("help")),
			Run:   func(cli.Context) error { return nil },
		}},
	}, "help")
}

func TestConformance_RequiredAfterOptional(t *testing.T) {
	ins := []*inputNode{
		parseInput(t, kindArg, "name=first type=string default=a"),
		parseInput(t, kindArg, "name=second type=string required=true"),
	}
	wantDiag(t, checkArgOrder("Migrate", ins), "follows an optional argument")

	libRejects(t, &cli.Command{
		Name: "app",
		Subcommands: []*cli.Command{{
			Name: "migrate",
			Args: cli.Args(cli.StringArg("first").Default("a"), cli.StringArg("second").Required()),
			Run:  func(cli.Context) error { return nil },
		}},
	}, "required")
}

func TestConformance_VariadicNotFinal(t *testing.T) {
	ins := []*inputNode{
		parseInput(t, kindArg, "name=rest type=strings variadic=true"),
		parseInput(t, kindArg, "name=last type=string required=true"),
	}
	wantDiag(t, checkArgOrder("Migrate", ins), "must be the final argument")

	libRejects(t, &cli.Command{
		Name: "app",
		Subcommands: []*cli.Command{{
			Name: "migrate",
			Args: cli.Args(cli.StringSliceArg("rest").Variadic(), cli.StringArg("last").Required()),
			Run:  func(cli.Context) error { return nil },
		}},
	}, "variadic")
}

func TestConformance_SliceArgWithoutVariadic(t *testing.T) {
	arg := &Input{fam: newFamily(), kind: kindArg}
	wantDiag(t, wireDiags(t, arg, "name=rest type=strings"), "variadic=true")

	libRejects(t, &cli.Command{
		Name: "app",
		Subcommands: []*cli.Command{{
			Name: "migrate",
			Args: cli.Args(cli.StringSliceArg("rest")),
			Run:  func(cli.Context) error { return nil },
		}},
	}, "")
}

func TestConformance_InheritedFlagCollision(t *testing.T) {
	fam := newFamily()
	rootDecl := fakeNode{id: 1}
	fam.root = &rootNode{decl: rootDecl}
	fam.inputs[rootDecl] = []*inputNode{parseInput(t, kindFlag, "name=timeout type=int default=1")}
	cmdDecl := fakeNode{id: 2}
	fam.commands = append(fam.commands, cmdReg{path: []string{"migrate"}, decl: cmdDecl, fn: "Migrate"})
	fam.inputs[cmdDecl] = []*inputNode{parseInput(t, kindFlag, "name=timeout type=int default=2")}
	wantDiag(t, fam.validateChains(), "collides with the inherited flag")

	libRejects(t, &cli.Command{
		Name:  "app",
		Flags: cli.Flags(cli.IntFlag("timeout")),
		Subcommands: []*cli.Command{{
			Name:  "migrate",
			Flags: cli.Flags(cli.IntFlag("timeout")),
			Run:   func(cli.Context) error { return nil },
		}},
	}, "timeout")
}

func TestConformance_InheritedShortCollision(t *testing.T) {
	fam := newFamily()
	rootDecl := fakeNode{id: 1}
	fam.root = &rootNode{decl: rootDecl}
	fam.inputs[rootDecl] = []*inputNode{parseInput(t, kindFlag, "name=verbose type=bool short=v")}
	cmdDecl := fakeNode{id: 2}
	fam.commands = append(fam.commands, cmdReg{path: []string{"status"}, decl: cmdDecl, fn: "Status"})
	fam.inputs[cmdDecl] = []*inputNode{parseInput(t, kindFlag, "name=vertical type=bool short=v")}
	wantDiag(t, fam.validateChains(), "share short -v")

	libRejects(t, &cli.Command{
		Name:  "app",
		Flags: cli.Flags(cli.BoolFlag("verbose").Short('v')),
		Subcommands: []*cli.Command{{
			Name:  "status",
			Flags: cli.Flags(cli.BoolFlag("vertical").Short('v')),
			Run:   func(cli.Context) error { return nil },
		}},
	}, "-v")
}

func TestConformance_VersionFlagReservation(t *testing.T) {
	fam := newFamily()
	rootDecl := fakeNode{id: 1}
	nd := &rootNode{decl: rootDecl, version: "1.0.0"}
	fam.root = nd
	fam.inputs[rootDecl] = []*inputNode{parseInput(t, kindFlag, "name=version type=bool")}
	r := &Root{fam: fam}
	wantDiag(t, r.Emit(nd, gen.New()), "version= reserves")

	root := &cli.Command{
		Name:    "app",
		Version: "1.0.0",
		Flags:   cli.Flags(cli.BoolFlag("version")),
		Subcommands: []*cli.Command{{
			Name: "status",
			Run:  func(cli.Context) error { return nil },
		}},
	}
	var errb strings.Builder
	code := root.Exec([]string{"status"}, cli.WithStderr(&errb), cli.WithStdout(io.Discard))
	if code == 0 || !strings.Contains(errb.String(), "version") {
		t.Errorf("library accepted a version flag beside Version: code=%d stderr=%q", code, errb.String())
	}
}

func TestConformance_SiblingAliasCollision(t *testing.T) {
	fam := newFamily()
	fam.commands = append(fam.commands,
		cmdReg{path: []string{"alpha"}, fn: "Alpha", aliases: []string{"beta"}},
		cmdReg{path: []string{"beta"}, fn: "Beta"},
	)
	wantDiag(t, fam.validateSiblingTokens(), "collides with a sibling")

	libRejects(t, &cli.Command{
		Name: "app",
		Subcommands: []*cli.Command{
			{Name: "alpha", Aliases: []string{"beta"}, Run: func(cli.Context) error { return nil }},
			{Name: "beta", Run: func(cli.Context) error { return nil }},
		},
	}, "duplicate")
}

// fakeNode distinguishes synthetic declaration keys in family maps.
type fakeNode struct{ id int }

func (fakeNode) Pos() token.Pos { return token.NoPos }
func (fakeNode) End() token.Pos { return token.NoPos }
