package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// noEnv keeps tests independent of the process environment.
func noEnv(string) (string, bool) { return "", false }

func envOf(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

// exec returns the exit code and captured output streams.
func exec(t *testing.T, root *Command, args []string, opts ...ExecOption) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	full := append([]ExecOption{
		WithStdout(&out),
		WithStderr(&errOut),
		withEnv(noEnv),
		WithSignalContext(context.Background()),
	}, opts...)
	code := root.Exec(args, full...)
	return code, out.String(), errOut.String()
}

func TestFlag_LongAndShort(t *testing.T) {
	port := IntFlag("port").Short('p').Default(8080)
	verbose := BoolFlag("verbose").Short('v')
	root := &Command{
		Name:  "myapp",
		Flags: Flags(port, verbose),
		Run: func(ctx Context) error {
			_, err := fmt.Fprintf(ctx.Stdout(), "port=%d verbose=%v\n", port.Get(ctx), verbose.Get(ctx))
			return err
		},
	}

	cases := []struct {
		args []string
		want string
	}{
		{[]string{}, "port=8080 verbose=false\n"},
		{[]string{"--port", "9090"}, "port=9090 verbose=false\n"},
		{[]string{"--port=9090"}, "port=9090 verbose=false\n"},
		{[]string{"-p", "9090"}, "port=9090 verbose=false\n"},
		{[]string{"-p=9090"}, "port=9090 verbose=false\n"},
		{[]string{"--verbose"}, "port=8080 verbose=true\n"},
		{[]string{"-v"}, "port=8080 verbose=true\n"},
		{[]string{"--verbose=false"}, "port=8080 verbose=false\n"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, "_"), func(t *testing.T) {
			code, stdout, stderr := exec(t, root, tc.args)
			if code != 0 {
				t.Fatalf("exit %d (stderr=%q)", code, stderr)
			}
			if stdout != tc.want {
				t.Errorf("stdout: want %q, got %q", tc.want, stdout)
			}
		})
	}
}

func TestFlag_Required(t *testing.T) {
	name := StringFlag("name").Required()
	root := &Command{
		Name:  "myapp",
		Flags: Flags(name),
		Run:   func(ctx Context) error { return nil },
	}
	code, _, stderr := exec(t, root, []string{})
	if code != 2 {
		t.Errorf("want exit 2, got %d", code)
	}
	if !strings.Contains(stderr, "required flag --name") {
		t.Errorf("stderr should mention required flag, got %q", stderr)
	}
}

func TestFlag_RequiredWithDefault_DoesNotError(t *testing.T) {
	port := IntFlag("port").Required().Default(8080)
	root := &Command{
		Name:  "myapp",
		Flags: Flags(port),
		Run: func(ctx Context) error {
			if port.Get(ctx) != 8080 {
				t.Errorf("want 8080, got %d", port.Get(ctx))
			}
			return nil
		},
	}
	if code, _, _ := exec(t, root, []string{}); code != 0 {
		t.Errorf("want exit 0, got %d", code)
	}
}

func TestFlag_Env(t *testing.T) {
	port := IntFlag("port").Env("PORT").Default(8080)
	root := &Command{
		Name:  "myapp",
		Flags: Flags(port),
		Run: func(ctx Context) error {
			_, err := fmt.Fprintf(ctx.Stdout(), "port=%d\n", port.Get(ctx))
			return err
		},
	}
	_, stdout, _ := exec(t, root, []string{}, withEnv(envOf(map[string]string{"PORT": "7777"})))
	if !strings.Contains(stdout, "port=7777") {
		t.Errorf("env not applied: %q", stdout)
	}
	_, stdout, _ = exec(t, root, []string{"--port", "9090"}, withEnv(envOf(map[string]string{"PORT": "7777"})))
	if !strings.Contains(stdout, "port=9090") {
		t.Errorf("explicit flag should win, got %q", stdout)
	}
}

func TestFlag_Validate_OneOf(t *testing.T) {
	level := StringFlag("level").OneOf("debug", "info", "warn", "error").Default("info")
	root := &Command{
		Name:  "myapp",
		Flags: Flags(level),
		Run:   func(ctx Context) error { return nil },
	}
	code, _, stderr := exec(t, root, []string{"--level", "trace"})
	if code != 2 {
		t.Errorf("want exit 2, got %d", code)
	}
	if !strings.Contains(stderr, "not one of") {
		t.Errorf("stderr should mention validation, got %q", stderr)
	}
}

func TestFlag_Validate_RunsAgainstDefault(t *testing.T) {
	run := func(f *Flag[string]) (int, string) {
		code, _, stderr := exec(t, &Command{Name: "myapp", Flags: Flags(f), Run: func(Context) error { return nil }}, []string{})
		return code, stderr
	}

	code, stderr := run(StringFlag("level").OneOf("debug", "info").Default("bogus"))
	if code != 2 {
		t.Errorf("invalid default: want exit 2, got %d", code)
	}
	if !strings.Contains(stderr, "not one of") {
		t.Errorf("want validation error, got %q", stderr)
	}

	if code, _ := run(StringFlag("level").OneOf("debug", "info").Default("info")); code != 0 {
		t.Errorf("valid default: want exit 0, got %d", code)
	}

	if code, _ := run(StringFlag("level").OneOf("debug", "info")); code != 0 {
		t.Errorf("unset no-default flag should not be validated: want exit 0, got %d", code)
	}
}

func TestFlag_Slice(t *testing.T) {
	items := StringSliceFlag("item")
	root := &Command{
		Name:  "myapp",
		Flags: Flags(items),
		Run: func(ctx Context) error {
			_, err := fmt.Fprintln(ctx.Stdout(), strings.Join(items.Get(ctx), ","))
			return err
		},
	}
	_, stdout, _ := exec(t, root, []string{"--item", "a", "--item", "b", "--item=c"})
	if strings.TrimSpace(stdout) != "a,b,c" {
		t.Errorf("slice flag accumulation: %q", stdout)
	}
}

func TestFlag_Lookup_DistinguishesExplicitFromDefault(t *testing.T) {
	to := IntFlag("to").Default(0)
	root := &Command{
		Name:  "myapp",
		Flags: Flags(to),
		Run: func(ctx Context) error {
			if _, set := to.Lookup(ctx); set {
				_, err := fmt.Fprintln(ctx.Stdout(), "set")
				return err
			}
			_, err := fmt.Fprintln(ctx.Stdout(), "default")
			return err
		},
	}
	_, stdout, _ := exec(t, root, []string{})
	if strings.TrimSpace(stdout) != "default" {
		t.Errorf("want default, got %q", stdout)
	}
	_, stdout, _ = exec(t, root, []string{"--to", "0"})
	if strings.TrimSpace(stdout) != "set" {
		t.Errorf("want set, got %q", stdout)
	}
}

func TestSubcommand_RoutingAndInheritedFlags(t *testing.T) {
	dsn := StringFlag("dsn").Required()
	var seen string
	migrate := &Command{
		Name: "migrate",
		Run: func(ctx Context) error {
			seen = dsn.Get(ctx)
			return nil
		},
	}
	db := &Command{
		Name:        "db",
		Flags:       Flags(dsn),
		Subcommands: []*Command{migrate},
	}
	root := &Command{Name: "myapp", Subcommands: []*Command{db}}

	code, _, stderr := exec(t, root, []string{"db", "migrate", "--dsn", "postgres://x"})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if seen != "postgres://x" {
		t.Errorf("parent flag not inherited, got %q", seen)
	}

	code, _, _ = exec(t, root, []string{"db", "--dsn", "postgres://y", "migrate"})
	if code != 0 {
		t.Errorf("flag-before-subcommand: exit %d", code)
	}
	if seen != "postgres://y" {
		t.Errorf("want y, got %q", seen)
	}
}

func TestSubcommand_GroupingOnlyShowsHelpOnBareInvocation(t *testing.T) {
	db := &Command{
		Name:        "db",
		Subcommands: []*Command{{Name: "status", Run: func(Context) error { return nil }}},
	}
	root := &Command{Name: "myapp", Subcommands: []*Command{db}}

	code, _, stderr := exec(t, root, []string{"db"})
	if code != 2 {
		t.Errorf("grouping-only bare exec: want exit 2, got %d", code)
	}
	if !strings.Contains(stderr, "Usage:") {
		t.Errorf("help should be rendered, got %q", stderr)
	}
	if strings.Contains(stderr, "myapp:") || strings.Contains(stderr, "missing subcommand") {
		t.Errorf("grouping-only bare exec should not print an error line, got %q", stderr)
	}
}

func TestSubcommand_TypoSuggestion(t *testing.T) {
	root := &Command{
		Name: "myapp",
		Subcommands: []*Command{
			{Name: "migrate", Run: func(Context) error { return nil }},
			{Name: "status", Run: func(Context) error { return nil }},
		},
	}
	code, _, stderr := exec(t, root, []string{"migrat"})
	if code != 2 {
		t.Errorf("unknown subcommand: want exit 2, got %d", code)
	}
	if !strings.Contains(stderr, "did you mean \"migrate\"?") {
		t.Errorf("want typo suggestion, got %q", stderr)
	}
}

func TestArgs_RequiredAndVariadic(t *testing.T) {
	host := StringArg("host").Required()
	extra := StringSliceArg("extra").Variadic()
	root := &Command{
		Name: "ssh",
		Args: Args(host, extra),
		Run: func(ctx Context) error {
			_, err := fmt.Fprintf(ctx.Stdout(), "host=%s extra=%v\n", host.Get(ctx), extra.Get(ctx))
			return err
		},
	}

	code, stdout, stderr := exec(t, root, []string{"example.com", "ls", "-l"})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "host=example.com") || !strings.Contains(stdout, "extra=[ls -l]") {
		t.Errorf("unexpected stdout: %q", stdout)
	}

	code, _, stderr = exec(t, root, []string{})
	if code != 2 {
		t.Errorf("missing required arg: want exit 2, got %d", code)
	}
	if !strings.Contains(stderr, "missing argument") {
		t.Errorf("want missing-arg error, got %q", stderr)
	}
}

func TestArgs_RequiredVariadicRejectsZero(t *testing.T) {
	files := StringSliceArg("files").Required().Variadic()
	root := &Command{Name: "cp", Args: Args(files), Run: func(Context) error { return nil }}

	code, _, stderr := exec(t, root, []string{})
	if code != 2 {
		t.Errorf("required variadic with no values: want exit 2, got %d", code)
	}
	if !strings.Contains(stderr, "missing argument") {
		t.Errorf("want missing-arg error, got %q", stderr)
	}

	if code, _, _ := exec(t, root, []string{"a", "b"}); code != 0 {
		t.Errorf("required variadic with values: want exit 0, got %d", code)
	}
}

func TestArgs_OptionalVariadicAcceptsZero(t *testing.T) {
	extra := StringSliceArg("extra").Variadic()
	root := &Command{Name: "ls", Args: Args(extra), Run: func(Context) error { return nil }}
	if code, _, stderr := exec(t, root, []string{}); code != 0 {
		t.Errorf("optional variadic with no values: want exit 0, got %d (%s)", code, stderr)
	}
}

func TestArg_OneOf(t *testing.T) {
	mode := StringArg("mode").OneOf("dev", "staging", "prod").Required()
	root := &Command{
		Name: "myapp",
		Args: Args(mode),
		Run: func(ctx Context) error {
			_, err := fmt.Fprintln(ctx.Stdout(), mode.Get(ctx))
			return err
		},
	}
	if code, stdout, _ := exec(t, root, []string{"staging"}); code != 0 || strings.TrimSpace(stdout) != "staging" {
		t.Errorf("accepted value: code=%d stdout=%q", code, stdout)
	}
	code, _, stderr := exec(t, root, []string{"qa"})
	if code != 2 {
		t.Errorf("disallowed value: want exit 2, got %d", code)
	}
	if !strings.Contains(stderr, "not one of") {
		t.Errorf("error should mention validation, got %q", stderr)
	}
}

func TestArg_Validate_RunsAgainstDefault(t *testing.T) {
	run := func(a *Arg[string]) (int, string) {
		code, _, stderr := exec(t, &Command{Name: "myapp", Args: Args(a), Run: func(Context) error { return nil }}, []string{})
		return code, stderr
	}

	code, stderr := run(StringArg("mode").OneOf("dev", "prod").Default("bogus"))
	if code != 2 {
		t.Errorf("invalid default: want exit 2, got %d", code)
	}
	if !strings.Contains(stderr, "not one of") {
		t.Errorf("want validation error, got %q", stderr)
	}

	if code, _ := run(StringArg("mode").OneOf("dev", "prod").Default("dev")); code != 0 {
		t.Errorf("valid default: want exit 0, got %d", code)
	}

	if code, _ := run(StringArg("mode").OneOf("dev", "prod")); code != 0 {
		t.Errorf("unset no-default arg should not be validated: want exit 0, got %d", code)
	}
}

func TestArgs_UnexpectedExtras(t *testing.T) {
	host := StringArg("host").Required()
	root := &Command{
		Name: "ssh",
		Args: Args(host),
		Run:  func(ctx Context) error { return nil },
	}
	code, _, stderr := exec(t, root, []string{"a", "b"})
	if code != 2 {
		t.Errorf("want exit 2, got %d", code)
	}
	if !strings.Contains(stderr, "unexpected argument") {
		t.Errorf("want unexpected-arg error, got %q", stderr)
	}
}

func TestArgs_SliceMustBeVariadic(t *testing.T) {
	// Invalid argument declarations fail during Exec as program errors.
	root := &Command{
		Name: "bad",
		Args: Args(StringSliceArg("rest")),
		Run:  func(Context) error { return nil },
	}
	code, _, stderr := exec(t, root, []string{"x"})
	if code != 1 {
		t.Errorf("want exit 1, got %d", code)
	}
	if !strings.Contains(stderr, "must be declared Variadic") {
		t.Errorf("want declaration-error message, got %q", stderr)
	}

	rest := StringSliceArg("rest").Variadic()
	good := &Command{
		Name: "good",
		Args: Args(rest),
		Run: func(ctx Context) error {
			_, err := fmt.Fprintln(ctx.Stdout(), strings.Join(rest.Get(ctx), ","))
			return err
		},
	}
	if code, stdout, _ := exec(t, good, []string{"a", "b", "c"}); code != 0 || strings.TrimSpace(stdout) != "a,b,c" {
		t.Errorf("variadic slice arg: code=%d stdout=%q", code, stdout)
	}
}

func TestArgs_VariadicMustBeLast(t *testing.T) {
	root := &Command{
		Name: "bad",
		Args: Args(StringSliceArg("rest").Variadic(), StringArg("after")),
		Run:  func(ctx Context) error { return nil },
	}
	code, _, stderr := exec(t, root, []string{"x"})
	if code != 1 {
		t.Errorf("want exit 1, got %d", code)
	}
	if !strings.Contains(stderr, "variadic arg") {
		t.Errorf("want declaration-error message, got %q", stderr)
	}
}

func TestArgs_VariadicMustBeSlice(t *testing.T) {
	root := &Command{Name: "bad", Args: Args(StringArg("x").Variadic()), Run: func(Context) error { return nil }}
	code, _, stderr := exec(t, root, []string{"a"})
	if code != 1 {
		t.Errorf("want exit 1, got %d", code)
	}
	if !strings.Contains(stderr, "must be a slice") {
		t.Errorf("want declaration-error message, got %q", stderr)
	}
}

func TestArgs_NilSubcommandIsDeclarationError(t *testing.T) {
	// Nil factories fail as declaration errors instead of panicking.
	root := &Command{Name: "app", Subcommands: []*Command{nil}, Run: func(Context) error { return nil }}
	code, _, stderr := exec(t, root, []string{})
	if code != 1 {
		t.Errorf("nil subcommand: want exit 1, got %d", code)
	}
	if !strings.Contains(stderr, "nil subcommand") {
		t.Errorf("want nil-subcommand declaration error, got %q", stderr)
	}
}

func TestArgs_DuplicateSubcommandIsError(t *testing.T) {
	root := &Command{
		Name: "app",
		Subcommands: []*Command{
			{Name: "serve", Run: func(Context) error { return nil }},
			{Name: "serve", Run: func(Context) error { return nil }},
		},
	}
	code, _, stderr := exec(t, root, []string{})
	if code != 1 || !strings.Contains(stderr, "duplicate subcommand") {
		t.Errorf("duplicate subcommand: code=%d stderr=%q", code, stderr)
	}
}

func TestArgs_DuplicateInheritedFlagIsError(t *testing.T) {
	// Inherited-name collisions would leave the child accessor unset.
	root := &Command{
		Name:  "app",
		Flags: Flags(StringFlag("mode")),
		Subcommands: []*Command{
			{Name: "run", Flags: Flags(StringFlag("mode")), Run: func(Context) error { return nil }},
		},
	}
	code, _, stderr := exec(t, root, []string{"run"})
	if code != 1 || !strings.Contains(stderr, "duplicate flag --mode") {
		t.Errorf("duplicate inherited flag: code=%d stderr=%q", code, stderr)
	}
}

func TestArgs_TypedNilFlagIsError(t *testing.T) {
	// A typed nil flag remains non-nil as an AnyFlag interface.
	var f *Flag[string]
	root := &Command{Name: "app", Flags: Flags(f), Run: func(Context) error { return nil }}
	code, _, stderr := exec(t, root, []string{})
	if code != 1 || !strings.Contains(stderr, "nil flag") {
		t.Errorf("typed-nil flag: code=%d stderr=%q", code, stderr)
	}
}

func TestArgs_ReservedFlagsAreErrors(t *testing.T) {
	cases := []struct {
		name string
		root *Command
		want string
	}{
		{"help-long", &Command{Name: "app", Flags: Flags(StringFlag("help")), Run: func(Context) error { return nil }}, "--help is reserved"},
		{"h-short", &Command{Name: "app", Flags: Flags(StringFlag("host").Short('h')), Run: func(Context) error { return nil }}, "-h is reserved"},
		{"version-with-version", &Command{Name: "app", Version: "1.0.0", Flags: Flags(StringFlag("version")), Run: func(Context) error { return nil }}, "--version is reserved"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := exec(t, tc.root, []string{})
			if code != 1 || !strings.Contains(stderr, tc.want) {
				t.Errorf("code=%d stderr=%q, want %q", code, stderr, tc.want)
			}
		})
	}

	// The version name is reserved only when the command defines Version.
	ok := &Command{Name: "app", Flags: Flags(StringFlag("version")), Run: func(Context) error { return nil }}
	if code, _, stderr := exec(t, ok, []string{}); code != 0 {
		t.Errorf("version flag without a declared Version should be allowed: code=%d stderr=%q", code, stderr)
	}

	// A child's Version also makes an inherited version flag unreachable.
	inherited := &Command{
		Name:  "app",
		Flags: Flags(StringFlag("version")),
		Subcommands: []*Command{
			{Name: "sub", Version: "1.0.0", Run: func(Context) error { return nil }},
		},
	}
	if code, _, stderr := exec(t, inherited, []string{"sub"}); code != 1 || !strings.Contains(stderr, "--version is reserved") {
		t.Errorf("inherited version flag with child Version: code=%d stderr=%q", code, stderr)
	}
}

func TestHelp_RootRendersFlagsAndSubcommands(t *testing.T) {
	port := IntFlag("port").Default(8080).Help("port to listen on")
	root := &Command{
		Name:  "myapp",
		Help:  "do useful things",
		Flags: Flags(port),
		Subcommands: []*Command{
			{Name: "serve", Help: "run the server", Run: func(Context) error { return nil }},
		},
	}
	code, stdout, _ := exec(t, root, []string{"--help"})
	if code != 0 {
		t.Errorf("want exit 0, got %d", code)
	}
	for _, want := range []string{"Usage:", "myapp", "--port", "port to listen on", "Commands:", "serve"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("help should contain %q, got:\n%s", want, stdout)
		}
	}
}

func TestHelp_SingleCommandOmitsCommandsSection(t *testing.T) {
	// Hidden completion commands do not make a command appear to have subcommands.
	root := &Command{
		Name: "greet",
		Help: "say hello",
		Args: Args(StringArg("name").Required()),
		Run:  func(Context) error { return nil },
	}
	code, stdout, _ := exec(t, root, []string{"--help"})
	if code != 0 {
		t.Fatalf("want exit 0, got %d", code)
	}
	if strings.Contains(stdout, "<command>") {
		t.Errorf("usage should not offer <command> without visible subcommands, got:\n%s", stdout)
	}
	if strings.Contains(stdout, "Commands:") {
		t.Errorf("help should not render a Commands section without visible subcommands, got:\n%s", stdout)
	}
	if strings.Contains(stdout, "for more information about a command") {
		t.Errorf("help should not print the subcommand guidance line, got:\n%s", stdout)
	}
}

func TestHelp_ChildRendersOwnHelpSummary(t *testing.T) {
	root := &Command{
		Name: "myapp",
		Subcommands: []*Command{
			{Name: "serve", Help: "run the HTTP server", Run: func(Context) error { return nil }},
		},
	}
	code, stdout, _ := exec(t, root, []string{"serve", "--help"})
	if code != 0 {
		t.Fatalf("want exit 0, got %d", code)
	}
	if !strings.Contains(stdout, "run the HTTP server") {
		t.Errorf("child --help should render the child's Help summary, got:\n%s", stdout)
	}
}

func TestHelp_RequiredVariadicRendersRequired(t *testing.T) {
	files := StringSliceArg("files").Required().Variadic()
	root := &Command{Name: "cp", Args: Args(files), Run: func(Context) error { return nil }}
	_, stdout, _ := exec(t, root, []string{"--help"})
	if !strings.Contains(stdout, "<files...>") {
		t.Errorf("required variadic should render <files...>, got:\n%s", stdout)
	}
	if strings.Contains(stdout, "[files...]") {
		t.Errorf("required variadic should not render as optional [files...], got:\n%s", stdout)
	}
}

func TestVersion(t *testing.T) {
	root := &Command{Name: "myapp", Version: "1.2.3", Run: func(Context) error { return nil }}
	code, stdout, _ := exec(t, root, []string{"--version"})
	if code != 0 {
		t.Errorf("want exit 0, got %d", code)
	}
	if strings.TrimSpace(stdout) != "1.2.3" {
		t.Errorf("want 1.2.3, got %q", stdout)
	}
}

func TestMiddleware_DeclarationOrder(t *testing.T) {
	var log []string
	mark := func(name string) Middleware {
		return func(next Handler) Handler {
			return func(ctx Context) error {
				log = append(log, "enter "+name)
				err := next(ctx)
				log = append(log, "leave "+name)
				return err
			}
		}
	}
	root := &Command{
		Name: "myapp",
		Use:  []Middleware{mark("A"), mark("B")},
		Run: func(ctx Context) error {
			log = append(log, "handler")
			return nil
		},
	}
	exec(t, root, []string{})
	want := []string{"enter A", "enter B", "handler", "leave B", "leave A"}
	if fmt.Sprint(log) != fmt.Sprint(want) {
		t.Errorf("middleware order:\nwant %v\ngot  %v", want, log)
	}
}

func TestMiddleware_InheritedFromParent(t *testing.T) {
	var hit bool
	mark := func(name string) Middleware {
		return func(next Handler) Handler {
			return func(ctx Context) error { hit = true; return next(ctx) }
		}
	}
	child := &Command{Name: "child", Run: func(Context) error { return nil }}
	root := &Command{
		Name:        "myapp",
		Use:         []Middleware{mark("root")},
		Subcommands: []*Command{child},
	}
	exec(t, root, []string{"child"})
	if !hit {
		t.Error("parent middleware should run for child commands")
	}
}

func TestExec_HandlerError_RendersAsExit1(t *testing.T) {
	root := &Command{
		Name: "myapp",
		Run:  func(Context) error { return errors.New("boom") },
	}
	code, _, stderr := exec(t, root, []string{})
	if code != 1 {
		t.Errorf("want exit 1, got %d", code)
	}
	if !strings.Contains(stderr, "myapp: boom") {
		t.Errorf("stderr should be prefixed with program name: %q", stderr)
	}
}

func TestExec_CustomOnError(t *testing.T) {
	root := &Command{
		Name: "myapp",
		Run:  func(Context) error { return errors.New("boom") },
	}
	code, _, stderr := exec(t, root, []string{},
		OnError(func(ctx Context, err error) int {
			//nolint:errcheck // The error renderer must return an exit code and has no channel for a stderr write failure.
			fmt.Fprintf(ctx.Stderr(), "CUSTOM:%s", err)
			return 42
		}),
	)
	if code != 42 {
		t.Errorf("want exit 42, got %d", code)
	}
	if !strings.Contains(stderr, "CUSTOM:boom") {
		t.Errorf("want CUSTOM:boom, got %q", stderr)
	}
}

func TestExec_ContextCanceledExitCode(t *testing.T) {
	root := &Command{
		Name: "myapp",
		Run:  func(Context) error { return context.Canceled },
	}
	code, _, _ := exec(t, root, []string{})
	if code != 130 {
		t.Errorf("want exit 130, got %d", code)
	}
}

func TestExec_UsageErrorTriggersHelpAppend(t *testing.T) {
	root := &Command{
		Name: "myapp",
		Run:  func(Context) error { return UsageError("you did it wrong") },
	}
	code, _, stderr := exec(t, root, []string{})
	if code != 2 {
		t.Errorf("want exit 2, got %d", code)
	}
	if !strings.Contains(stderr, "Usage:") {
		t.Errorf("usage error should append help: %q", stderr)
	}
}

func TestExec_CtxCancellationPropagates(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled

	root := &Command{
		Name: "myapp",
		Run: func(ctx Context) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
				return errors.New("never reached")
			}
		},
	}
	code, _, _ := exec(t, root, []string{}, WithSignalContext(parent))
	if code != 130 {
		t.Errorf("want exit 130 for cancelled ctx, got %d", code)
	}
}

func TestParse_StandaloneInspection(t *testing.T) {
	port := IntFlag("port").Default(8080)
	serve := &Command{Name: "serve", Flags: Flags(port), Run: func(Context) error { return nil }}
	root := &Command{Name: "myapp", Subcommands: []*Command{serve}}

	res, err := root.Parse([]string{"serve", "--port", "9090"})
	if err != nil {
		t.Fatal(err)
	}
	if res.cmd != serve {
		t.Errorf("resolved wrong command")
	}
	if got := res.values[port]; got != 9090 {
		t.Errorf("port: want 9090, got %v", got)
	}
}
