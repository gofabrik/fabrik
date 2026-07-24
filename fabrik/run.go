package main

import (
	"fmt"
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
	cmd, err := runCommand(dir, passthrough)
	if err != nil {
		return err
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// runCommand prepares generation and execution from the module root so the
// full module is scanned and generated paths resolve correctly.
func runCommand(dir string, passthrough []string) (*exec.Cmd, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	root, err := moduleRoot(abs)
	if err != nil {
		return nil, err
	}
	mainDir, err := wire(root)
	if err != nil {
		return nil, err
	}
	goArgs := append([]string{"run", mainPackageArg(root, mainDir)}, passthrough...)
	cmd := exec.Command("go", goArgs...) // #nosec G204 G702 -- launches the go toolchain with controlled args and no shell
	cmd.Dir = root
	cmd.Env = runEnv()
	return cmd, nil
}

// runEnv defaults FABRIK_ENV to development unless it is already present, including with an empty value.
func runEnv() []string {
	if _, ok := os.LookupEnv("FABRIK_ENV"); ok {
		return nil
	}
	return append(os.Environ(), "FABRIK_ENV=development")
}

func moduleRoot(dir string) (string, error) {
	for d := dir; ; {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d, nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", fmt.Errorf("no go.mod found in %s or any parent directory", dir)
		}
		d = parent
	}
}
