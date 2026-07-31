package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gofabrik/fabrik/fabrik/internal/genconfig"
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
	opts, resolved, err := resolveOptions(root, genconfig.Overrides{})
	if err != nil {
		return nil, err
	}

	if resolved.Emit == genconfig.EmitEmbedded {
		return nil, fmt.Errorf("fabrik run does not run embedded output; the host project builds and runs it")
	}
	mainDir, err := wireWith(root, opts)
	if err != nil {
		return nil, err
	}
	goArgs := runArgs(resolved.BuildTag, mainPackageArg(root, mainDir), passthrough)
	cmd := exec.Command("go", goArgs...) // #nosec G204 G702 -- launches the go toolchain with controlled args and no shell
	cmd.Dir = root
	cmd.Env = runEnv()
	return cmd, nil
}

// runArgs includes the configured build tag in the go run arguments.
func runArgs(buildTag, pkg string, passthrough []string) []string {
	args := []string{"run"}
	if buildTag != "" {
		args = append(args, "-tags="+buildTag)
	}
	args = append(args, pkg)
	return append(args, passthrough...)
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
