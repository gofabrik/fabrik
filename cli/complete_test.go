package cli

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type failingWriter struct {
	err error
}

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestTimeFlag_ParsesRFC3339(t *testing.T) {
	when := TimeFlag("when")
	root := &Command{
		Name:  "myapp",
		Flags: Flags(when),
		Run:   func(Context) error { return nil },
	}
	code, _, stderr := exec(t, root, []string{"--when", "2024-03-15T10:30:00Z"})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	want := time.Date(2024, 3, 15, 10, 30, 0, 0, time.UTC)
	res, _ := root.Parse([]string{"--when", "2024-03-15T10:30:00Z"})
	got, ok := res.values[when]
	if !ok {
		t.Fatal("when not present in values")
	}
	if !got.(time.Time).Equal(want) {
		t.Errorf("want %v, got %v", want, got)
	}
}

func TestTimeFlag_RejectsNonRFC3339(t *testing.T) {
	when := TimeFlag("when")
	root := &Command{Name: "myapp", Flags: Flags(when), Run: func(Context) error { return nil }}
	code, _, stderr := exec(t, root, []string{"--when", "yesterday"})
	if code != 2 {
		t.Errorf("want exit 2, got %d", code)
	}
	if !strings.Contains(stderr, "--when") {
		t.Errorf("error should reference the flag, got %q", stderr)
	}
}

func TestCompletion_ScriptsEmitNonEmpty(t *testing.T) {
	root := &Command{Name: "myapp", Run: func(Context) error { return nil }}
	for _, shell := range []string{"bash", "zsh", "fish"} {
		t.Run(shell, func(t *testing.T) {
			code, stdout, stderr := exec(t, root, []string{"completion", shell})
			if code != 0 {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			if !strings.Contains(stdout, "myapp") {
				t.Errorf("%s script should mention program name, got %q", shell, stdout)
			}
			if !strings.Contains(stdout, "__complete") {
				t.Errorf("%s script should call back via __complete, got %q", shell, stdout)
			}
		})
	}
}

func TestCompletion_ReportsWriteError(t *testing.T) {
	writeErr := errors.New("write failed")
	root := &Command{Name: "myapp", Run: func(Context) error { return nil }}
	var reported error
	code, _, _ := exec(t, root, []string{"completion", "bash"},
		WithStdout(failingWriter{err: writeErr}),
		OnError(func(_ Context, err error) int {
			reported = err
			return 42
		}),
	)
	if code != 42 {
		t.Fatalf("want write failure exit code 42, got %d", code)
	}
	if !errors.Is(reported, writeErr) {
		t.Fatalf("want reported error %v, got %v", writeErr, reported)
	}
}

func TestCompletion_UnsupportedShell(t *testing.T) {
	root := &Command{Name: "myapp", Run: func(Context) error { return nil }}
	code, _, stderr := exec(t, root, []string{"completion", "pwsh"})
	if code != 2 {
		t.Errorf("want exit 2, got %d", code)
	}
	if !strings.Contains(stderr, "pwsh") || !strings.Contains(stderr, "bash") {
		t.Errorf("stderr should name the rejected value and the allowed shells, got %q", stderr)
	}
}

func TestComplete_SubcommandCandidates(t *testing.T) {
	root := &Command{
		Name: "myapp",
		Subcommands: []*Command{
			{Name: "serve", Run: func(Context) error { return nil }},
			{Name: "migrate", Run: func(Context) error { return nil }},
			{Name: "status", Run: func(Context) error { return nil }},
		},
	}
	// Hidden injected commands never appear as candidates.
	_, stdout, _ := exec(t, root, []string{"__complete", "--", ""})
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	want := map[string]bool{"serve": false, "migrate": false, "status": false}
	for _, l := range lines {
		if _, ok := want[l]; ok {
			want[l] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("completion should suggest %q (got %v)", name, lines)
		}
	}
	for _, hidden := range []string{"completion", "__complete"} {
		for _, l := range lines {
			if l == hidden {
				t.Errorf("auto-injected %q should not appear in candidate list (got %v)", hidden, lines)
			}
		}
	}
}

func TestComplete_WorksWithRequiredRootFlag(t *testing.T) {
	// Runtime completion bypasses inherited required flags.
	token := StringFlag("token").Required()
	root := &Command{
		Name:  "myapp",
		Flags: Flags(token),
		Subcommands: []*Command{
			{Name: "serve", Run: func(Context) error { return nil }},
			{Name: "status", Run: func(Context) error { return nil }},
		},
		Run: func(Context) error { return nil },
	}
	code, stdout, stderr := exec(t, root, []string{"__complete", "--", ""})
	if code != 0 {
		t.Fatalf("completion should exit 0 with an unset required flag, got %d (stderr=%q)", code, stderr)
	}
	for _, want := range []string{"serve", "status"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("want %q candidate, got %q", want, stdout)
		}
	}
}

func TestCompletion_ScriptWorksWithRequiredRootFlag(t *testing.T) {
	token := StringFlag("token").Required()
	root := &Command{
		Name:  "myapp",
		Flags: Flags(token),
		Run:   func(Context) error { return nil },
	}
	code, stdout, stderr := exec(t, root, []string{"completion", "bash"})
	if code != 0 {
		t.Fatalf("completion script should emit with an unset required flag, got %d (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stdout, "__complete") {
		t.Errorf("completion script should be emitted, got %q", stdout)
	}
}

func TestCompletion_UserCommandStillEnforcesRequired(t *testing.T) {
	// Policy bypass depends on injection, not the command name.
	token := StringFlag("token").Required()
	root := &Command{
		Name:  "myapp",
		Flags: Flags(token),
		Subcommands: []*Command{
			{Name: "completion", Run: func(Context) error { return nil }},
		},
	}
	code, _, stderr := exec(t, root, []string{"completion"})
	if code != 2 {
		t.Fatalf("user's own completion command should still enforce required flags, got exit %d", code)
	}
	if !strings.Contains(stderr, "required flag --token") {
		t.Errorf("want required-flag error, got %q", stderr)
	}
}

func TestCompletion_SkipsInheritedMiddleware(t *testing.T) {
	// Injected completion commands run outside application middleware.
	var ran bool
	mw := func(next Handler) Handler {
		return func(ctx Context) error { ran = true; return next(ctx) }
	}
	root := &Command{
		Name: "myapp",
		Use:  []Middleware{mw},
		Subcommands: []*Command{
			{Name: "serve", Run: func(Context) error { return nil }},
		},
	}
	if code, _, stderr := exec(t, root, []string{"__complete", "--", ""}); code != 0 {
		t.Fatalf("__complete: want exit 0, got %d (%s)", code, stderr)
	}
	if ran {
		t.Error("app middleware must not run for the injected __complete command")
	}

	ran = false
	exec(t, root, []string{"serve"})
	if !ran {
		t.Error("app middleware should run for a real command")
	}
}

func TestCompletion_SkipsInheritedFlagValidation(t *testing.T) {
	// Injected commands bypass inherited environment resolution and validation.
	mode := StringFlag("mode").Env("APP_MODE").OneOf("dev", "prod")
	root := &Command{
		Name:  "myapp",
		Flags: Flags(mode),
		Subcommands: []*Command{
			{Name: "serve", Run: func(Context) error { return nil }},
		},
	}
	env := withEnv(envOf(map[string]string{"APP_MODE": "bogus"}))
	if code, stdout, stderr := exec(t, root, []string{"__complete", "--", ""}, env); code != 0 || !strings.Contains(stdout, "serve") {
		t.Fatalf("__complete with invalid env-bound flag: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if code, _, _ := exec(t, root, []string{"serve"}, env); code != 2 {
		t.Errorf("real command should reject the invalid env-bound flag, got exit %d", code)
	}
}

func TestComplete_PrefixFiltering(t *testing.T) {
	root := &Command{
		Name: "myapp",
		Subcommands: []*Command{
			{Name: "serve", Run: func(Context) error { return nil }},
			{Name: "migrate", Run: func(Context) error { return nil }},
		},
	}
	_, stdout, _ := exec(t, root, []string{"__complete", "--", "se"})
	if !strings.Contains(stdout, "serve") {
		t.Errorf("want serve in output, got %q", stdout)
	}
	if strings.Contains(stdout, "migrate") {
		t.Errorf("migrate should not match prefix 'se', got %q", stdout)
	}
}

func TestComplete_LongFlagCandidates(t *testing.T) {
	port := IntFlag("port").Default(8080)
	verbose := BoolFlag("verbose")
	root := &Command{Name: "myapp", Flags: Flags(port, verbose), Run: func(Context) error { return nil }}

	_, stdout, _ := exec(t, root, []string{"__complete", "--", "--p"})
	if !strings.Contains(stdout, "--port") {
		t.Errorf("want --port suggestion, got %q", stdout)
	}
	if strings.Contains(stdout, "--verbose") {
		t.Errorf("--verbose should not match prefix --p, got %q", stdout)
	}
}

func TestComplete_DashSuggestsAllFlags(t *testing.T) {
	port := IntFlag("port").Short('p').Default(8080)
	verbose := BoolFlag("verbose").Short('v')
	root := &Command{Name: "myapp", Flags: Flags(port, verbose), Run: func(Context) error { return nil }}

	_, stdout, _ := exec(t, root, []string{"__complete", "--", "-"})
	for _, want := range []string{"-p", "--port", "-v", "--verbose"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("want %q in suggestions, got %q", want, stdout)
		}
	}
}

func TestComplete_FlagValueAfterSpace(t *testing.T) {
	mode := StringFlag("mode").OneOf("dev", "staging", "prod")
	root := &Command{Name: "myapp", Flags: Flags(mode), Run: func(Context) error { return nil }}

	_, stdout, _ := exec(t, root, []string{"__complete", "--", "--mode", ""})
	for _, want := range []string{"dev", "staging", "prod"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("want %q in suggestions, got %q", want, stdout)
		}
	}

	_, stdout, _ = exec(t, root, []string{"__complete", "--", "--mode", "s"})
	if !strings.Contains(stdout, "staging") {
		t.Errorf("want staging in suggestions, got %q", stdout)
	}
	if strings.Contains(stdout, "dev") || strings.Contains(stdout, "prod") {
		t.Errorf("only 'staging' should match prefix 's', got %q", stdout)
	}
}

func TestComplete_FlagValueInline(t *testing.T) {
	mode := StringFlag("mode").OneOf("dev", "staging", "prod")
	root := &Command{Name: "myapp", Flags: Flags(mode), Run: func(Context) error { return nil }}

	_, stdout, _ := exec(t, root, []string{"__complete", "--", "--mode="})
	for _, want := range []string{"--mode=dev", "--mode=staging", "--mode=prod"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("want %q in suggestions, got %q", want, stdout)
		}
	}

	_, stdout, _ = exec(t, root, []string{"__complete", "--", "--mode=s"})
	if !strings.Contains(stdout, "--mode=staging") {
		t.Errorf("want --mode=staging, got %q", stdout)
	}
	if strings.Contains(stdout, "--mode=dev") || strings.Contains(stdout, "--mode=prod") {
		t.Errorf("only --mode=staging should match prefix s, got %q", stdout)
	}
}

func TestComplete_FlagValueShortForm(t *testing.T) {
	mode := StringFlag("mode").Short('m').OneOf("dev", "staging", "prod")
	root := &Command{Name: "myapp", Flags: Flags(mode), Run: func(Context) error { return nil }}

	_, stdout, _ := exec(t, root, []string{"__complete", "--", "-m", ""})
	for _, want := range []string{"dev", "staging", "prod"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("want %q in suggestions, got %q", want, stdout)
		}
	}
}

func TestComplete_VariadicLocksFlagsAfterPositional(t *testing.T) {
	// Completion must not suggest flags that a variadic would consume as values.
	port := IntFlag("port").Default(22)
	root := &Command{
		Name:  "ssh",
		Flags: Flags(port),
		Args:  Args(StringArg("host").Required(), StringSliceArg("extra").Variadic()),
		Run:   func(Context) error { return nil },
	}
	_, stdout, _ := exec(t, root, []string{"__complete", "--", "example.com", "--p"})
	if strings.Contains(stdout, "--port") {
		t.Errorf("variadic command: --port should NOT be suggested after positional, got %q", stdout)
	}
}

func TestComplete_NonVariadicAllowsFlagsAfterPositional(t *testing.T) {
	// Non-variadic commands keep flags available after positionals.
	port := IntFlag("port").Default(8080)
	root := &Command{
		Name:  "serve",
		Flags: Flags(port),
		Args:  Args(StringArg("host").Required()),
		Run:   func(Context) error { return nil },
	}
	_, stdout, _ := exec(t, root, []string{"__complete", "--", "example.com", "--p"})
	if !strings.Contains(stdout, "--port") {
		t.Errorf("non-variadic command: --port should still be suggested after positional, got %q", stdout)
	}
}

func TestParse_FlagAfterPositional_NonVariadic(t *testing.T) {
	port := IntFlag("port").Default(8080)
	host := StringArg("host").Required()
	root := &Command{
		Name:  "serve",
		Flags: Flags(port),
		Args:  Args(host),
		Run: func(ctx Context) error {
			_, err := fmt.Fprintf(ctx.Stdout(), "host=%s port=%d\n", host.Get(ctx), port.Get(ctx))
			return err
		},
	}
	code, stdout, stderr := exec(t, root, []string{"example.com", "--port", "9090"})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "host=example.com port=9090") {
		t.Errorf("unexpected stdout: %q", stdout)
	}
}

func TestComplete_PositionalLocked_KeepsPositionalSuggestions(t *testing.T) {
	rest := StringSliceArg("rest").Variadic().Complete(StaticValues("alpha", "bravo"))
	root := &Command{
		Name: "myapp",
		Args: Args(rest),
		Run:  func(Context) error { return nil },
	}
	_, stdout, _ := exec(t, root, []string{"__complete", "--", "alpha", "b"})
	if !strings.Contains(stdout, "bravo") {
		t.Errorf("variadic completer should still suggest, got %q", stdout)
	}
}

func TestComplete_HonoursDoubleDashSeparator(t *testing.T) {
	// The separator stops flag parsing without occupying an argument slot.
	a := StringArg("a").Required().Complete(StaticValues("alpha-only"))
	b := StringArg("b").Required().Complete(StaticValues("bravo-only"))
	root := &Command{
		Name: "myapp",
		Args: Args(a, b),
		Run:  func(Context) error { return nil },
	}

	_, stdout, _ := exec(t, root, []string{"__complete", "--", "--", "arg1", ""})
	if !strings.Contains(stdout, "bravo-only") {
		t.Errorf("after `-- arg1` cur should target arg b, got %q", stdout)
	}
	if strings.Contains(stdout, "alpha-only") {
		t.Errorf("slot a is already filled, should not suggest, got %q", stdout)
	}

	_, stdout, _ = exec(t, root, []string{"__complete", "--", "--", ""})
	if !strings.Contains(stdout, "alpha-only") {
		t.Errorf("after `--` alone cur should target arg a, got %q", stdout)
	}
}

func TestComplete_BoolFlagInlineSuggestsTrueFalse(t *testing.T) {
	verbose := BoolFlag("verbose")
	root := &Command{Name: "myapp", Flags: Flags(verbose), Run: func(Context) error { return nil }}

	_, stdout, _ := exec(t, root, []string{"__complete", "--", "--verbose="})
	for _, want := range []string{"--verbose=true", "--verbose=false"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("want %q in suggestions, got %q", want, stdout)
		}
	}

	_, stdout, _ = exec(t, root, []string{"__complete", "--", "--verbose=t"})
	if !strings.Contains(stdout, "--verbose=true") {
		t.Errorf("want --verbose=true, got %q", stdout)
	}
	if strings.Contains(stdout, "--verbose=false") {
		t.Errorf("--verbose=false should not match prefix 't', got %q", stdout)
	}
}

func TestComplete_ArgCompleter(t *testing.T) {
	shell := StringArg("shell").Complete(StaticValues("bash", "zsh", "fish"))
	root := &Command{
		Name: "myapp",
		Args: Args(shell),
		Run:  func(Context) error { return nil },
	}
	_, stdout, _ := exec(t, root, []string{"__complete", "--", "b"})
	if !strings.Contains(stdout, "bash") {
		t.Errorf("want bash suggestion for prefix 'b', got %q", stdout)
	}
	if strings.Contains(stdout, "zsh") {
		t.Errorf("zsh should not match prefix 'b', got %q", stdout)
	}
}

func TestHelp_AutoInjectedCompletionIsHidden(t *testing.T) {
	root := &Command{
		Name: "myapp",
		Subcommands: []*Command{
			{Name: "serve", Help: "run the server", Run: func(Context) error { return nil }},
		},
	}
	_, stdout, _ := exec(t, root, []string{"--help"})
	if !strings.Contains(stdout, "serve") {
		t.Errorf("user's subcommand should appear in help, got:\n%s", stdout)
	}
	for _, hidden := range []string{"completion", "__complete"} {
		if strings.Contains(stdout, hidden) {
			t.Errorf("auto-injected %q should not appear in help, got:\n%s", hidden, stdout)
		}
	}
}

func TestExec_DoesNotMutateUserCommandTree(t *testing.T) {
	root := &Command{
		Name: "myapp",
		Subcommands: []*Command{
			{Name: "serve", Run: func(Context) error { return nil }},
		},
	}
	before := len(root.Subcommands)
	beforeAddr := &root.Subcommands[0]

	exec(t, root, []string{"serve"})
	exec(t, root, []string{"serve"})
	exec(t, root, []string{"serve"})

	if len(root.Subcommands) != before {
		t.Errorf("Exec mutated user's Subcommands (was %d, now %d)", before, len(root.Subcommands))
	}
	if &root.Subcommands[0] != beforeAddr {
		t.Errorf("Exec mutated the user's Subcommands slice backing array")
	}
}

func TestExec_ConcurrentInvocationsAreSafe(t *testing.T) {
	root := &Command{
		Name: "myapp",
		Subcommands: []*Command{
			{Name: "serve", Run: func(Context) error { return nil }},
		},
	}
	const workers = 16
	done := make(chan struct{}, workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			exec(t, root, []string{"serve"})
		}()
	}
	for i := 0; i < workers; i++ {
		<-done
	}
	if len(root.Subcommands) != 1 {
		t.Errorf("concurrent Exec mutated user's tree: %d subcommands", len(root.Subcommands))
	}
}

func TestHelp_NoColorOnBuffer(t *testing.T) {
	// Non-file writers never receive terminal escape sequences.
	root := &Command{
		Name:  "myapp",
		Flags: Flags(IntFlag("port").Default(8080)),
		Run:   func(Context) error { return nil },
	}
	_, stdout, _ := exec(t, root, []string{"--help"})
	if strings.Contains(stdout, "\033[") {
		t.Errorf("non-terminal writer should never emit ANSI escapes, got:\n%s", stdout)
	}
}
