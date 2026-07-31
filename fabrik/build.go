package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/gofabrik/fabrik/fabrik/internal/genconfig"
)

func buildCmd(args []string) error {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	out := fs.String("o", "", "output binary path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir := "."
	if fs.NArg() > 0 {
		dir = fs.Arg(0)
		// Accept flags after the optional directory.
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return err
		}
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q; usage: fabrik build [dir] [-o out]", fs.Arg(0))
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	opts, resolved, err := resolveOptions(abs, genconfig.Overrides{})
	if err != nil {
		return err
	}

	if resolved.Emit == genconfig.EmitEmbedded {
		return fmt.Errorf("fabrik build does not build embedded output; the host project builds it")
	}
	mainDir, err := wireWith(abs, opts)
	if err != nil {
		return err
	}

	goArgs := buildArgs(*out, resolved.BuildTag, mainPackageArg(abs, mainDir))

	cmd := exec.Command("go", goArgs...) // #nosec G204 -- launches the go toolchain with controlled args
	cmd.Dir = abs
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// buildArgs includes the configured build tag in the go build arguments.
func buildArgs(out, buildTag, pkg string) []string {
	args := []string{"build"}
	if out != "" {
		args = append(args, "-o", out)
	}
	if buildTag != "" {
		args = append(args, "-tags="+buildTag)
	}
	return append(args, pkg)
}
