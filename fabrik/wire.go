package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofabrik/fabrik/fabrik/internal/codegen"
	"github.com/gofabrik/fabrik/fabrik/internal/diag"
	"github.com/gofabrik/fabrik/fabrik/internal/scan"
)

func wireCmd(args []string) error {
	fs := flag.NewFlagSet("wire", flag.ContinueOnError)
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
	return wire(abs)
}

func wire(dir string) error {
	project, diags, err := scan.Scan(dir)
	if err != nil {
		return err
	}

	// Only run codegen on a clean scan: generating from a partially parsed
	// project produces misleading follow-on diagnostics.
	var src []byte
	if !diags.HasFatal() {
		s, gdiags, gerr := codegen.Generate(project)
		if gerr != nil {
			return gerr
		}
		src = s
		diags = append(diags, gdiags...)
	}

	diags.Sort()
	if len(diags) > 0 {
		f := diag.NewFormatter(os.Stderr)
		for _, d := range diags {
			f.Emit(d)
		}
		errs, warns := diags.Counts()
		f.Summary(errs, warns)
		if diags.HasFatal() {
			return errSilent
		}
	}

	out := filepath.Join(project.MainDir, "main.gen.go")
	if err := os.WriteFile(out, src, 0o644); err != nil {
		return err
	}
	if rel, rerr := filepath.Rel(dir, out); rerr == nil {
		fmt.Printf("fabrik: wrote %s\n", rel)
	} else {
		fmt.Printf("fabrik: wrote %s\n", out)
	}
	return nil
}
