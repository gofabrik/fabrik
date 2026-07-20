package gen

import (
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/diag"
)

func TestCommandDispatchBuildsAndRuns(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped under -short")
	}

	g := New()
	g.SetModule("fixture")
	pkg := typecheckScopePkg(t, "fixture/dep", "package dep\n\ntype Store struct{}\n\ntype Extra struct{}\n")
	storeT := types.NewPointer(pkg.Scope().Lookup("Store").Type())
	extraT := types.NewPointer(pkg.Scope().Lookup("Extra").Type())

	g.BindLazy(storeT, "", func() (string, diag.Diagnostics) {
		v := g.Var("store")
		c := g.Var(v + "Close")
		g.Node(&Call{
			Base:    Base{Phase: PhaseWire},
			Var:     v,
			Fn:      "newStore",
			Args:    []string{g.Context()},
			Err:     ErrReturn,
			Cleanup: c,
		})
		return v, nil
	})
	g.BindLazy(extraT, "", func() (string, diag.Diagnostics) {
		v := g.Var("extra")
		g.Node(&Call{Base: Base{Phase: PhaseWire}, Var: v, Fn: "newExtra", Err: ErrReturn})
		return v, nil
	})

	greet := g.AddScope("buildGreet", token.Position{}, storeT)
	g.AddCommandFunc(CommandFunc{Name: "greet", Help: "Greet", Fn: "greet", Scope: greet, Pos: token.Position{}})
	other := g.AddScope("buildOther", token.Position{}, extraT)
	g.AddCommandFunc(CommandFunc{Name: "other", Fn: "other", Scope: other, Pos: token.Position{}})

	if ds := g.MaterializeScopes(); ds.HasFatal() {
		t.Fatalf("MaterializeScopes: %v", ds)
	}
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
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("main.gen.go", string(out))
	write("main.go", "package main\n\nimport \"os\"\n\nfunc main() { os.Exit(run()) }\n")
	write("support.go", fixtureSupport)
	write("dep/dep.go", "package dep\n\ntype Store struct{}\n\ntype Extra struct{}\n")
	write("go.mod", "module fixture\n\ngo 1.26\n\nrequire github.com/gofabrik/fabrik/cli v0.1.0\n\nreplace github.com/gofabrik/fabrik/cli => "+cliPath+"\n")

	goEnv := append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	tidy.Env = goEnv
	if b, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, b)
	}
	bin := filepath.Join(dir, "fixture")
	// #nosec G204 -- the command and all arguments are controlled by this test
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = dir
	build.Env = goEnv
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s\n--- generated ---\n%s", err, b, out)
	}

	run := func(args []string, env ...string) (int, string, string) {
		// #nosec G204 -- the binary path and arguments are controlled by this test
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

	if code, _, se := run([]string{"--help"}, "FAIL_STORE=1"); code != 0 || strings.Contains(se, "CONSTRUCTED") {
		t.Errorf("--help: code=%d stderr=%q (must not construct)", code, se)
	}
	if code, so, se := run([]string{"__complete", "--", ""}, "FAIL_STORE=1"); code != 0 || !strings.Contains(so, "greet") || strings.Contains(se, "CONSTRUCTED") {
		t.Errorf("__complete: code=%d stdout=%q stderr=%q", code, so, se)
	}

	if code, so, _ := run([]string{"greet"}); code != 0 || !strings.Contains(so, "hello store\nstore closed\n") {
		t.Errorf("greet: code=%d stdout=%q (want hello then cleanup)", code, so)
	}

	if code, _, se := run([]string{"other"}); code != 0 || strings.Contains(se, "CONSTRUCTED store") || !strings.Contains(se, "CONSTRUCTED extra") {
		t.Errorf("other: code=%d stderr=%q (want extra constructed, store not)", code, se)
	}

	if code, so, se := run([]string{"greet"}, "FAIL_STORE=1"); code != 1 || !strings.Contains(se, "fixture: store failed") || strings.Contains(so, "store closed") {
		t.Errorf("FAIL_STORE greet: code=%d stdout=%q stderr=%q", code, so, se)
	}

	if code, _, se := run([]string{"greet"}, "FORCE_CANCEL=1"); code != 130 || strings.Contains(se, "fixture:") {
		t.Errorf("FORCE_CANCEL greet: code=%d stderr=%q (want exit 130, no error line)", code, se)
	}

	if code, so, se := run([]string{}); code != 2 || !strings.Contains(so+se, "greet") {
		t.Errorf("bare: code=%d stdout=%q stderr=%q (want command listing, exit 2)", code, so, se)
	}
}

const fixtureSupport = `package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"fixture/dep"

	"github.com/gofabrik/fabrik/cli"
)

func newStore(ctx context.Context) (*dep.Store, func(), error) {
	fmt.Fprintln(os.Stderr, "CONSTRUCTED store")
	if os.Getenv("FORCE_CANCEL") != "" {
		p, _ := os.FindProcess(os.Getpid())
		_ = p.Signal(os.Interrupt)
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}
	if os.Getenv("FAIL_STORE") != "" {
		return nil, nil, errors.New("store failed")
	}
	return &dep.Store{}, func() { fmt.Println("store closed") }, nil
}

func newExtra() (*dep.Extra, error) {
	fmt.Fprintln(os.Stderr, "CONSTRUCTED extra")
	return &dep.Extra{}, nil
}

func greet(ctx cli.Context, s *dep.Store) error {
	fmt.Fprintln(ctx.Stdout(), "hello store")
	return nil
}

func other(ctx cli.Context, e *dep.Extra) error {
	return nil
}
`
