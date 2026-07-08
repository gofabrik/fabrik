package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	mainDir, err := wire(abs)
	if err != nil {
		return err
	}

	goArgs := []string{"build"}
	if *out != "" {
		goArgs = append(goArgs, "-o", *out)
	}
	goArgs = append(goArgs, mainPackageArg(abs, mainDir))

	cmd := exec.Command("go", goArgs...)
	cmd.Dir = abs
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
