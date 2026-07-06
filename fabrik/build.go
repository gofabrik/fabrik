package main

import (
	"flag"
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
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if err := wire(abs); err != nil {
		return err
	}

	goArgs := []string{"build"}
	if *out != "" {
		goArgs = append(goArgs, "-o", *out)
	}
	goArgs = append(goArgs, ".")

	cmd := exec.Command("go", goArgs...)
	cmd.Dir = abs
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
