package gen

import (
	"strings"
	"testing"
)

func TestAddCommandRegistersCallExpr(t *testing.T) {
	g := New()
	if g.CommandCount() != 0 {
		t.Fatalf("CommandCount = %d, want 0", g.CommandCount())
	}
	g.AddCommand("shared.GreetCommand(store)")
	g.AddCommand("shared.SeedCommand(store)")
	if g.CommandCount() != 2 {
		t.Fatalf("CommandCount = %d, want 2", g.CommandCount())
	}
	if g.commands[0] != "shared.GreetCommand(store)" {
		t.Errorf("commands[0] = %q", g.commands[0])
	}
}

func TestRenderCommandDispatch(t *testing.T) {
	g := New()
	g.SetModule("demo")
	g.Stmt(PhaseConfig, "loadConfig()")
	g.Stmt(PhasePrepare, "migrateDB(ctx)")
	g.AddCommand("shared.GreetCommand()")

	out, err := g.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	src := string(out)
	for _, want := range []string{
		"func run() int {",
		"err := func() error {",
		"return 130",
		`fmt.Fprintln(os.Stderr, "demo:", err)`,
		"return 1",
		`Name: "demo"`,
		"Run: func(cli.Context) error {",
		"shared.GreetCommand()",
		"return root.Exec(os.Args[1:], cli.WithSignalContext(ctx))",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("Render output missing %q:\n%s", want, src)
		}
	}
	for _, absent := range []string{"errc :=", "ecancel()", "func HTTPServer"} {
		if strings.Contains(src, absent) {
			t.Errorf("default handler must start nothing, but found %q:\n%s", absent, src)
		}
	}

	// Graph construction precedes the root; prepare stays inside the default handler.
	idxConfig := strings.Index(src, "loadConfig()")
	idxRoot := strings.Index(src, "root = &cli.Command{")
	idxDefaultRun := strings.Index(src, "Run: func(cli.Context) error {")
	idxPrepare := strings.Index(src, "migrateDB(ctx)")
	if idxConfig < 0 || idxRoot < 0 || idxConfig > idxRoot {
		t.Errorf("config should run before the root is built (config=%d root=%d)", idxConfig, idxRoot)
	}
	if idxDefaultRun < 0 || idxPrepare < 0 || idxPrepare < idxDefaultRun {
		t.Errorf("prepare hook must be inside the default handler (defaultRun=%d prepare=%d)", idxDefaultRun, idxPrepare)
	}
}

func TestRenderPlainReturnsError(t *testing.T) {
	g := New()
	g.SetModule("demo")
	g.Stmt(PhaseConfig, "setup()")

	out, err := g.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	src := string(out)
	if !strings.Contains(src, "func run() error {") {
		t.Errorf("plain run() should return error, got:\n%s", src)
	}
	if strings.Contains(src, "func run() int {") {
		t.Errorf("plain run() should not be the command shape, got:\n%s", src)
	}
}
