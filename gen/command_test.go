package gen

import (
	"bytes"
	"go/token"
	"go/types"
	"regexp"
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

// Command inputs share handles between tree fields and wrapper bindings.
func TestRenderCommandWithInputs(t *testing.T) {
	w := newScopeWorld(t)
	g := w.g
	s := g.AddScope("buildMigrate", token.Position{}, w.store)
	dry := g.Var("migrateDryRun")
	dir := g.Var("migrateDirection")
	g.AddCommandFunc(CommandFunc{
		Name: "migrate",
		Help: "Apply migrations",
		Long: "Applies every pending migration.",
		Fn:   "app.Migrate",
		Inputs: []CommandInput{
			{Var: dry, Builder: `cli.BoolFlag("dry-run").Short('n')`},
			{Var: dir, Builder: `cli.StringArg("direction").Default("up")`, Arg: true},
		},
		ValueExprs: []string{dir + ".Get(ctx)", dry + ".Get(ctx)"},
		Examples:   []CommandExample{{Cmd: "app migrate up", Help: "Apply all."}},
		Scope:      s,
	})

	if ds := g.MaterializeScopes(); ds.HasFatal() {
		t.Fatalf("MaterializeScopes: %v", ds)
	}
	out, err := g.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	src := string(out)
	for _, want := range []string{
		"migrateDryRun := cli.BoolFlag(\"dry-run\").Short('n')",
		`migrateDirection := cli.StringArg("direction").Default("up")`,
		"Flags: cli.Flags(migrateDryRun),",
		"Args:  cli.Args(migrateDirection),",
		`"Applies every pending migration.",`,
		"Examples: []cli.Example{",
		`{Cmd: "app migrate up", Help: "Apply all."},`,
		"migrateDirection.Get(ctx), migrateDryRun.Get(ctx))",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("output missing %q\n%s", want, src)
		}
	}
}

// Shared path prefixes produce ordered intermediates that may also be executable.
func TestRenderNestedCommandPaths(t *testing.T) {
	g := New()
	for _, c := range []struct {
		path []string
		fn   string
	}{
		{[]string{"status"}, "app.Status"},
		{[]string{"database", "migrate"}, "app.Migrate"},
		{[]string{"database", "reset"}, "app.Reset"},
		{[]string{"jobs", "status"}, "app.JobsStatus"},
		{[]string{"database"}, "app.Database"},
	} {
		s := g.AddScope("build"+c.fn[4:], token.Position{})
		g.AddCommandFunc(CommandFunc{Name: c.path[len(c.path)-1], Path: c.path, Fn: c.fn, Scope: s})
	}
	if ds := g.MaterializeScopes(); ds.HasFatal() {
		t.Fatalf("MaterializeScopes: %v", ds)
	}
	out, err := g.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	src := string(out)
	status := strings.Index(src, `Name: "status",`)
	database := strings.Index(src, `Name: "database",`)
	migrate := strings.Index(src, `Name: "migrate",`)
	reset := strings.Index(src, `Name: "reset",`)
	jobs := strings.Index(src, `Name: "jobs",`)
	if !(status < database && database < migrate && migrate < reset && reset < jobs) {
		t.Errorf("sibling order must follow first contribution: status=%d database=%d migrate=%d reset=%d jobs=%d\n%s",
			status, database, migrate, reset, jobs, src)
	}
	if !strings.Contains(src, "app.Database(ctx)") {
		t.Errorf("executable parent database lost its Run:\n%s", src)
	}
	if strings.Count(src, `Name: "status",`) != 2 {
		t.Errorf("status must appear under root and under jobs:\n%s", src)
	}
	dbRun := strings.Index(src, "app.Database(ctx)")
	if !(database < migrate && migrate < reset && reset < dbRun) {
		t.Errorf("database children must render inside the database node before its Run: db=%d migrate=%d reset=%d dbRun=%d", database, migrate, reset, dbRun)
	}
	tree := buildCommandTree(g.commandFuncs, nil, nil)
	var jobsNode *commandNode
	for _, n := range tree {
		if n.name == "jobs" {
			jobsNode = n
		}
	}
	if jobsNode == nil || jobsNode.cmd != nil || len(jobsNode.children) != 1 ||
		jobsNode.children[0].name != "status" || jobsNode.children[0].cmd == nil {
		t.Fatalf("bare intermediate jobs malformed: %+v", jobsNode)
	}
	if !strings.Contains(src, "app.JobsStatus(ctx)") {
		t.Errorf("nested jobs status lost its wrapper:\n%s", src)
	}
}

// Group, root, and command metadata render on their respective nodes.
func TestRenderGroupAndRootSpecs(t *testing.T) {
	g := New()
	rootFlag := g.Var("rootVerbose")
	g.SetCommandRoot(RootSpec{
		Usage:   "app <command>",
		Version: "1.2.3",
		Long:    "The app long description.",
		Inputs:  []CommandInput{{Var: rootFlag, Builder: `cli.BoolFlag("verbose")`}},
	})
	dbFlag := g.Var("databaseTimeout")
	g.AddCommandGroup(CommandGroup{
		Path:     []string{"database"},
		Help:     "Database maintenance.",
		Usage:    "app database <command>",
		Aliases:  []string{"db"},
		Hidden:   true,
		Inputs:   []CommandInput{{Var: dbFlag, Builder: `cli.IntFlag("timeout")`}},
		Examples: []CommandExample{{Cmd: "app db migrate", Help: "Via the alias."}},
	})
	s := g.AddScope("buildMigrate", token.Position{})
	g.AddCommandFunc(CommandFunc{
		Name: "migrate", Path: []string{"database", "migrate"},
		Usage: "app database migrate", Aliases: []string{"m"},
		Fn: "app.Migrate", Scope: s,
		ValueExprs: []string{dbFlag + ".Get(ctx)", rootFlag + ".Get(ctx)"},
	})
	if ds := g.MaterializeScopes(); ds.HasFatal() {
		t.Fatalf("MaterializeScopes: %v", ds)
	}
	out, err := g.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	src := regexp.MustCompile(`[ \t]+`).ReplaceAllString(string(out), " ")
	for _, want := range []string{
		`Usage: "app <command>",`,
		`Version: "1.2.3",`,
		`Long: "The app long description.",`,
		"Flags: cli.Flags(rootVerbose),",
		`rootVerbose := cli.BoolFlag("verbose")`,
		`databaseTimeout := cli.IntFlag("timeout")`,
		`Help: "Database maintenance.",`,
		`Usage: "app database <command>",`,
		`Aliases: []string{"db"},`,
		"Flags: cli.Flags(databaseTimeout),",
		`{Cmd: "app db migrate", Help: "Via the alias."},`,
		`Usage: "app database migrate",`,
		`Aliases: []string{"m"},`,
		"databaseTimeout.Get(ctx), rootVerbose.Get(ctx))",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("output missing %q\n%s", want, src)
		}
	}
	if strings.Count(src, "Hidden: true,") != 1 {
		t.Errorf("exactly the group carries Hidden, got:\n%s", src)
	}
}

// Sibling order follows the earliest group or command contribution.
func TestBuildCommandTreeInterleavedOrder(t *testing.T) {
	g := New()
	g.AddCommandGroup(CommandGroup{Path: []string{"beta"}})
	s := g.AddScope("buildAlpha", token.Position{})
	g.AddCommandFunc(CommandFunc{Name: "alpha", Fn: "app.Alpha", Scope: s})
	tree := buildCommandTree(g.commandFuncs, g.commandGroups, g.treeOrder)
	if len(tree) != 2 || tree[0].name != "beta" || tree[1].name != "alpha" {
		names := []string{}
		for _, n := range tree {
			names = append(names, n.name)
		}
		t.Fatalf("sibling order = %v, want [beta alpha]", names)
	}
}

// Middleware preserves declaration order on root, group, and command nodes.
func TestRenderMiddlewareChains(t *testing.T) {
	g := New()
	g.SetCommandRoot(RootSpec{Use: []string{"app.Audit"}})
	g.AddCommandGroup(CommandGroup{Path: []string{"database"}, Use: []string{"app.Confirm", "app.Retry"}})
	s := g.AddScope("buildMigrate", token.Position{})
	g.AddCommandFunc(CommandFunc{
		Name: "migrate", Path: []string{"database", "migrate"},
		Fn: "app.Migrate", Scope: s, Use: []string{"app.Retry"},
	})
	if ds := g.MaterializeScopes(); ds.HasFatal() {
		t.Fatalf("MaterializeScopes: %v", ds)
	}
	out, err := g.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	src := regexp.MustCompile(`[ \t]+`).ReplaceAllString(string(out), " ")
	for _, want := range []string{
		"Use: []cli.Middleware{app.Audit},",
		"Use: []cli.Middleware{app.Confirm, app.Retry},",
		"Use: []cli.Middleware{app.Retry},",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("output missing %q\n%s", want, src)
		}
	}
}

// A root spec without commands still emits a parseable CLI tree.
func TestRenderRootOnlySurface(t *testing.T) {
	g := New()
	g.SetCommandRoot(RootSpec{Version: "1.2.3", Long: "The app."})
	out, err := g.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	src := regexp.MustCompile(`[ \t]+`).ReplaceAllString(string(out), " ")
	for _, want := range []string{`Version: "1.2.3",`, "root.Exec(os.Args[1:]"} {
		if !strings.Contains(src, want) {
			t.Errorf("output missing %q\n%s", want, src)
		}
	}
}
