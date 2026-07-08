package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func runCmd(args []string) error {
	dir := "."
	var passthrough []string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		dir = args[0]
		passthrough = args[1:]
	} else {
		passthrough = args
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	mainDir, err := wire(abs)
	if err != nil {
		return err
	}

	goArgs := append([]string{"run", mainPackageArg(abs, mainDir)}, passthrough...)
	cmd := exec.Command("go", goArgs...)
	cmd.Dir = abs
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
