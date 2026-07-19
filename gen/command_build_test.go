package gen

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCommandDispatchBuildsAndRuns pins command lifecycle and exit-code behavior in a built binary.
func TestCommandDispatchBuildsAndRuns(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped under -short")
	}

	g := New()
	g.SetModule("fixture")
	g.Stmt(PhaseConfig, "if os.Getenv(\"FORCE_CANCEL\") != \"\" {\ncancel()\nreturn ctx.Err()\n}")
	g.Stmt(PhaseConfig, "cfg, err := loadConfig()\nif err != nil {\nreturn err\n}")
	g.Stmt(PhaseWire, "store := NewStore(cfg)")
	g.RecordVarType("store", "*Store")
	g.Stmt(PhasePrepare, "if err := migrate(ctx, store); err != nil {\nreturn err\n}")
	g.AddEntrypoint("HTTPServer", []string{"store"}, []string{
		`if os.Getenv("FAIL_SERVER") != "" {`, `return errors.New("server failed")`, `}`,
		"<-ctx.Done()", "return nil",
	})
	g.AddEntrypoint("JobWorker", []string{"store"}, []string{"<-ctx.Done()", "return nil"})
	g.AddCommand("greetCommand(store)")

	out, err := g.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	cliPath, err := filepath.Abs("../cli")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("main.gen.go", string(out))
	write("main.go", "package main\n\nimport \"os\"\n\nfunc main() { os.Exit(run()) }\n")
	write("support.go", fixtureSupport)
	write("go.mod", "module fixture\n\ngo 1.26\n\nrequire github.com/gofabrik/fabrik/cli v0.1.0\n\nreplace github.com/gofabrik/fabrik/cli => "+cliPath+"\n")

	goEnv := append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	tidy.Env = goEnv
	if b, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, b)
	}
	bin := filepath.Join(dir, "fixture")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = dir
	build.Env = goEnv
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s\n--- generated ---\n%s", err, b, out)
	}

	run := func(args []string, env ...string) (int, string, string) {
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), env...)
		var so, se strings.Builder
		cmd.Stdout = &so
		cmd.Stderr = &se
		runErr := cmd.Run()
		code := 0
		if ee, ok := runErr.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if runErr != nil {
			t.Fatalf("run %v: %v", args, runErr)
		}
		return code, so.String(), se.String()
	}

	// Commands bypass prepare hooks.
	if code, so, se := run([]string{"greet", "alice"}); code != 0 || !strings.Contains(so, "hello alice") || strings.Contains(se, "MIGRATED") {
		t.Errorf("greet: code=%d stdout=%q stderr=%q (prepare must not run)", code, so, se)
	}

	// Help bypasses prepare hooks.
	if code, _, se := run([]string{"--help"}); code != 0 || strings.Contains(se, "MIGRATED") {
		t.Errorf("--help: code=%d stderr=%q (prepare must not run)", code, se)
	}

	if code, _, _ := run([]string{"greet"}); code != 2 {
		t.Errorf("greet missing arg: want exit 2, got %d", code)
	}

	// Setup failures stop before prepare.
	if code, _, se := run([]string{}, "FAIL_CONFIG=1"); code != 1 || strings.Contains(se, "MIGRATED") {
		t.Errorf("FAIL_CONFIG: code=%d stderr=%q", code, se)
	}

	// Graph construction failures block commands and help before dispatch.
	if code, so, _ := run([]string{"greet", "alice"}, "FAIL_CONFIG=1"); code != 1 || strings.Contains(so, "hello") {
		t.Errorf("FAIL_CONFIG greet: want exit 1 with no command output, got %d stdout=%q", code, so)
	}
	if code, _, _ := run([]string{"--help"}, "FAIL_CONFIG=1"); code != 1 {
		t.Errorf("FAIL_CONFIG --help: want exit 1 (setup blocks help), got %d", code)
	}

	// Setup cancellation exits 130 without rendering an error or running prepare.
	if code, _, se := run([]string{}, "FORCE_CANCEL=1"); code != 130 || strings.Contains(se, "fixture:") || strings.Contains(se, "MIGRATED") {
		t.Errorf("FORCE_CANCEL: code=%d stderr=%q (want exit 130, no error line)", code, se)
	}

	// The default path runs prepare and cancels peer entrypoints after a failure.
	if code, _, se := run([]string{}, "FAIL_SERVER=1"); code != 1 || !strings.Contains(se, "MIGRATED") || !strings.Contains(se, "server failed") {
		t.Errorf("FAIL_SERVER default: code=%d stderr=%q (want prepare + cancellation + exit 1)", code, se)
	}
}

const fixtureSupport = `package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/gofabrik/fabrik/cli"
)

type Config struct{}

func loadConfig() (Config, error) {
	if os.Getenv("FAIL_CONFIG") != "" {
		return Config{}, errors.New("config failed")
	}
	return Config{}, nil
}

type Store struct{}

func NewStore(cfg Config) *Store { return &Store{} }

func migrate(ctx context.Context, s *Store) error {
	fmt.Fprintln(os.Stderr, "MIGRATED")
	return nil
}

func greetCommand(s *Store) *cli.Command {
	name := cli.StringArg("name").Required()
	return &cli.Command{
		Name: "greet",
		Help: "Greet someone",
		Args: cli.Args(name),
		Run: func(ctx cli.Context) error {
			fmt.Fprintf(ctx.Stdout(), "hello %s\n", name.Get(ctx))
			return nil
		},
	}
}
`
