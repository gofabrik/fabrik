package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/fabrik/internal/diagfmt"
	"github.com/gofabrik/fabrik/fabrik/internal/engine"
)

func wireCmd(args []string) error {
	fs := flag.NewFlagSet("wire", flag.ContinueOnError)
	check := fs.Bool("check", false, "verify main.gen.go is up to date instead of writing it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir := "."
	if fs.NArg() > 0 {
		dir = fs.Arg(0)
	}
	if fs.NArg() > 1 {
		return fmt.Errorf("unexpected argument %q; usage: fabrik wire [-check] [dir]", fs.Arg(1))
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if *check {
		return wireCheck(abs)
	}
	_, err = wire(abs)
	return err
}

// generate reports diagnostics and returns errSilent on fatal ones.
func generate(dir string) (src []byte, out string, err error) {
	res, err := engine.Wire(dir, nil)
	if err != nil {
		if res != nil && len(res.Diags) > 0 {
			if writeErr := emitDiagnostics(res.Diags); writeErr != nil {
				return nil, "", writeErr
			}
		}
		return nil, "", err
	}
	if len(res.Diags) > 0 {
		if err := emitDiagnostics(res.Diags); err != nil {
			return nil, "", err
		}
		if res.Diags.HasFatal() {
			return nil, "", errSilent
		}
	}
	return res.Src, filepath.Join(res.MainDir, "main.gen.go"), nil
}

func emitDiagnostics(diags diag.Diagnostics) error {
	f := diagfmt.NewFormatter(os.Stderr)
	for _, d := range diags {
		if err := f.Emit(d); err != nil {
			return err
		}
	}
	return f.Summary(diags.Counts())
}

// wire writes main.gen.go and returns the main package directory.
func wire(dir string) (string, error) {
	src, out, err := generate(dir)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(out, src, 0o644); err != nil { // #nosec G306 -- generated Go source is intentionally readable
		return "", err
	}
	if rel, rerr := filepath.Rel(dir, out); rerr == nil {
		fmt.Printf("fabrik: wrote %s\n", rel)
	} else {
		fmt.Printf("fabrik: wrote %s\n", out)
	}
	return filepath.Dir(out), nil
}

// mainPackageArg renders the go command target for the main package.
func mainPackageArg(dir, mainDir string) string {
	rel, err := filepath.Rel(dir, mainDir)
	if err != nil || rel == "." {
		return "."
	}
	return "./" + filepath.ToSlash(rel)
}

// wireCheck fails when main.gen.go is missing or stale.
func wireCheck(dir string) error {
	src, out, err := generate(dir)
	if err != nil {
		return err
	}
	disk, err := os.ReadFile(out) // #nosec G304 -- reads an app/workspace-selected path
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "fabrik: %s does not exist; run fabrik wire\n", out)
			return errSilent
		}
		return err
	}
	if !bytes.Equal(disk, src) {
		fmt.Fprintf(os.Stderr, "fabrik: %s is stale; run fabrik wire\n", out)
		return errSilent
	}
	fmt.Printf("fabrik: main.gen.go up to date\n")
	return nil
}
