package gen

import (
	"bytes"
	"go/token"
	"go/types"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/diag"
)

func TestAddCommandFuncRegistersShell(t *testing.T) {
	g := New()
	if g.CommandCount() != 0 {
		t.Fatalf("CommandCount = %d, want 0", g.CommandCount())
	}
	s := g.AddScope("buildGreet", token.Position{})
	g.AddCommandFunc(CommandFunc{Name: "greet", Help: "Greet", Fn: "shared.Greet", Scope: s})
	if g.CommandCount() != 1 {
		t.Fatalf("CommandCount = %d, want 1", g.CommandCount())
	}
	if g.commandFuncs[0].Name != "greet" {
		t.Errorf("commandFuncs[0] = %+v", g.commandFuncs[0])
	}
}

// Command dispatch must not construct dependencies before selection.
func TestRenderCommandShellTree(t *testing.T) {
	w := newScopeWorld(t)
	g := w.g
	s := g.AddScope("buildServe", token.Position{}, w.store)
	g.AddCommandFunc(CommandFunc{Name: "serve", Help: "Start the server", Fn: "app.Serve", Scope: s})
	empty := g.AddScope("buildVersion", token.Position{})
	g.AddCommandFunc(CommandFunc{Name: "version", Fn: "app.Version", Scope: empty})

	if ds := g.MaterializeScopes(); ds.HasFatal() {
		t.Fatalf("MaterializeScopes: %v", ds)
	}
	out, err := g.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	src := string(out)

	for _, want := range []string{
		"func run() int {",
		`Name: "demo",`,
		`Name: "serve",`,
		`Help: "Start the server",`,
		"Run: func(ctx cli.Context) error {",
		"store, cleanup, err := buildServe(ctx)",
		"defer cleanup()",
		"return app.Serve(ctx, store)",
		`Name: "version",`,
		"return app.Version(ctx)",
		"return root.Exec(os.Args[1:], cli.WithSignalContext(ctx))",
		"func buildServe(ctx context.Context) (*app.Store, func(), error) {",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("shell tree missing %q:\n%s", want, src)
		}
	}
	for _, absent := range []string{
		"err := func() error {",
		"return 130",
		"fmt.",
		"errors.",
		"func buildVersion",
	} {
		if strings.Contains(src, absent) {
			t.Errorf("shell tree must not contain %q:\n%s", absent, src)
		}
	}
	if strings.Index(src, "root := &cli.Command{") > strings.Index(src, "func buildServe") {
		t.Errorf("build functions must follow run():\n%s", src)
	}
}

func TestRenderCommandSetupOnlyScope(t *testing.T) {
	w := newScopeWorld(t)
	g := w.g
	g.ScopePrologue(func() diag.Diagnostics {
		g.Node(&Call{Base: Base{Phase: PhaseSetup}, Fn: "app.InitLogger", Err: ErrInline})
		return nil
	})
	s := g.AddScope("buildVersion", token.Position{})
	g.AddCommandFunc(CommandFunc{Name: "version", Fn: "app.Version", Scope: s})
	if ds := g.MaterializeScopes(); ds.HasFatal() {
		t.Fatalf("MaterializeScopes: %v", ds)
	}
	out, err := g.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	src := string(out)
	for _, want := range []string{
		"func buildVersion(ctx context.Context) error {",
		"if err := buildVersion(ctx); err != nil {",
		"return app.Version(ctx)",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("setup-only scope missing %q:\n%s", want, src)
		}
	}
}

func TestRenderRepeatedCommandShape(t *testing.T) {
	w := newScopeWorld(t)
	g := w.g
	s := g.AddScope("buildServe", token.Position{}, w.store)
	g.AddCommandFunc(CommandFunc{Name: "serve", Fn: "app.Serve", Scope: s})
	if ds := g.MaterializeScopes(); ds.HasFatal() {
		t.Fatalf("MaterializeScopes: %v", ds)
	}
	first, err := g.Render()
	if err != nil {
		t.Fatalf("first render: %v", err)
	}
	second, err := g.Render()
	if err != nil {
		t.Fatalf("second render: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("repeated render differs:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

func TestWrapperVars(t *testing.T) {
	pkg := typecheckScopePkg(t, "example.com/w", `package w

type Server struct{}

type DB struct{}

type DB2 struct{}
`)
	server := types.NewPointer(pkg.Scope().Lookup("Server").Type())
	db := types.NewPointer(pkg.Scope().Lookup("DB").Type())
	db2t := types.NewPointer(pkg.Scope().Lookup("DB2").Type())
	g := New()
	s := &Scope{roots: []types.Type{server, db, db, db2t}}
	got := wrapperVars(g, s)
	want := []string{"server", "db", "db2", "db22"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("wrapperVars = %v, want %v", got, want)
		}
	}
}

func TestDepVarBaseInitialisms(t *testing.T) {
	pkg := typecheckScopePkg(t, "example.com/n", `package n

type HTTPConfig struct{}

type DB struct{}

type Server struct{}
`)
	cases := map[string]string{"HTTPConfig": "httpConfig", "DB": "db", "Server": "server"}
	for name, want := range cases {
		typ := types.NewPointer(pkg.Scope().Lookup(name).Type())
		if got := depVarBase(typ); got != want {
			t.Errorf("depVarBase(%s) = %q, want %q", name, got, want)
		}
	}
}

func TestWrapperVarsAvoidImportAliases(t *testing.T) {
	pkg := typecheckScopePkg(t, "example.com/app", "package app\n\ntype App struct{}\n")
	appT := types.NewPointer(pkg.Scope().Lookup("App").Type())
	g := New()
	g.ImportPkg(pkg)
	s := &Scope{roots: []types.Type{appT}}
	if got := wrapperVars(g, s); got[0] != "app2" {
		t.Fatalf("wrapperVars = %v, want app2 (alias app is taken)", got)
	}
}

func TestScopeReservesWrapperSkeletonNames(t *testing.T) {
	g := New()
	g.AddScope("buildX", token.Position{})
	if a := g.Import("example.com/cleanup"); a == "cleanup" {
		t.Fatalf("cleanup alias = %q, must avoid the wrapper skeleton name", a)
	}
	if a := g.Import("example.com/err"); a == "err" {
		t.Fatalf("err alias = %q, must avoid the wrapper skeleton name", a)
	}
	if a := g.Import("example.com/ctx"); a == "ctx" {
		t.Fatalf("ctx alias = %q, must avoid the wrapper param name", a)
	}
}
