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
	g.AddEntrypoint("HTTPServer", []string{"srv"}, []string{"return nil"})
	g.RecordVarType("srv", "*Server")
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
		"Run: func(cctx cli.Context) error {",
		"ectx, ecancel := context.WithCancel(cctx)",
		"ecancel()",
		"shared.GreetCommand()",
		"return root.Exec(os.Args[1:], cli.WithSignalContext(ctx))",
		"func HTTPServer(ctx context.Context, srv *Server) error",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("Render output missing %q:\n%s", want, src)
		}
	}

	// Graph construction precedes the root, while prepare remains inside its default handler.
	idxConfig := strings.Index(src, "loadConfig()")
	idxRoot := strings.Index(src, "root = &cli.Command{")
	idxDefaultRun := strings.Index(src, "Run: func(cctx cli.Context) error {")
	idxPrepare := strings.Index(src, "migrateDB(ctx)")
	if idxConfig < 0 || idxRoot < 0 || idxConfig > idxRoot {
		t.Errorf("config should run before the root is built (config=%d root=%d)", idxConfig, idxRoot)
	}
	if idxDefaultRun < 0 || idxPrepare < 0 || idxPrepare < idxDefaultRun {
		t.Errorf("prepare hook must be inside the default handler, not the setup closure (defaultRun=%d prepare=%d)", idxDefaultRun, idxPrepare)
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

func TestRenderEntrypointOnlyStillReturnsError(t *testing.T) {
	g := New()
	g.SetModule("demo")
	g.AddEntrypoint("HTTPServer", []string{"srv"}, []string{"return nil"})
	g.RecordVarType("srv", "*Server")

	out, err := g.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	src := string(out)
	if !strings.Contains(src, "func run() (err error) {") {
		t.Errorf("entrypoint-only run() should return error, got:\n%s", src)
	}
	if strings.Contains(src, "func run() int {") {
		t.Errorf("entrypoint-only run() should not be the command shape, got:\n%s", src)
	}
}
