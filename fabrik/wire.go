package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/fabrik/internal/diagfmt"
	"github.com/gofabrik/fabrik/fabrik/internal/engine"
	"github.com/gofabrik/fabrik/fabrik/internal/genconfig"
	"github.com/gofabrik/fabrik/gen"
)

func wireCmd(args []string) error {
	dir, check, ov, graph, err := parseWireArgs(args)
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	opts, _, err := resolveOptions(abs, ov)
	if err != nil {
		return err
	}
	opts.Graph = graph
	if check {
		return wireCheck(abs, opts)
	}
	_, err = wireWith(abs, opts)
	return err
}

func parseWireArgs(args []string) (dir string, check bool, ov genconfig.Overrides, graph bool, err error) {
	fs := flag.NewFlagSet("wire", flag.ContinueOnError)
	checkFlag := fs.Bool("check", false, "verify main.gen.go is up to date instead of writing it")
	comments := fs.String("comments", "", "generated comment level: off, sections, or full (overrides fabrik.yaml)")
	graphFlag := fs.Bool("graph", false, "write the structural graph beside main.gen.go")
	if err := fs.Parse(args); err != nil {
		return "", false, genconfig.Overrides{}, false, err
	}
	commentsSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "comments" {
			commentsSet = true
		}
	})
	if commentsSet {
		level, err := commentLevel(*comments)
		if err != nil {
			return "", false, genconfig.Overrides{}, false, err
		}
		ov.Comments = &level
	}
	if *checkFlag && *graphFlag {
		return "", false, genconfig.Overrides{}, false, fmt.Errorf("-check does not write files; drop --graph to check, or drop -check to write the graph")
	}
	dir = "."
	if fs.NArg() > 0 {
		dir = fs.Arg(0)
	}
	if fs.NArg() > 1 {
		return "", false, genconfig.Overrides{}, false, fmt.Errorf("unexpected argument %q; usage: fabrik wire [-check] [-comments=LEVEL] [--graph] [dir]", fs.Arg(1))
	}
	return dir, *checkFlag, ov, *graphFlag, nil
}

// resolveOptions merges fabrik.yaml with per-invocation overrides into engine
// options, reporting genconfig diagnostics through the same path as engine
// diagnostics; a fatal one returns errSilent.
func resolveOptions(dir string, ov genconfig.Overrides) (engine.Options, genconfig.Options, error) {
	resolved, diags := genconfig.Resolve(dir, ov)
	if len(diags) > 0 {
		if err := emitDiagnostics(diags); err != nil {
			return engine.Options{}, genconfig.Options{}, err
		}
		if diags.HasFatal() {
			return engine.Options{}, genconfig.Options{}, errSilent
		}
	}
	return resolved.EngineOptions(), resolved, nil
}

func commentLevel(s string) (gen.CommentLevel, error) {
	if s == "" {
		return 0, fmt.Errorf("invalid -comments level %q: want off, sections, or full", s)
	}
	level, err := genconfig.ParseCommentLevel(s)
	if err != nil {
		return 0, fmt.Errorf("invalid -comments level %q: want off, sections, or full", s)
	}
	return level, nil
}

// generate reports diagnostics and returns errSilent on fatal ones.
func generate(dir string, opts engine.Options) (res *engine.Result, out string, err error) {
	res, err = engine.WireOptions(dir, nil, opts)
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
	return res, filepath.Join(res.OutDir, "main.gen.go"), nil
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
	return wireWith(dir, engine.Options{})
}

func wireWith(dir string, opts engine.Options) (string, error) {
	res, out, err := generate(dir, opts)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(out, res.Src, 0o644); err != nil { // #nosec G306 -- generated Go source is intentionally readable
		return "", err
	}
	written := []string{out}
	if res.Graph != nil {
		sidecars, err := writeGraphSidecars(filepath.Dir(out), res.Graph)
		if err != nil {
			return "", err
		}
		written = append(written, sidecars...)
	}
	for _, path := range written {
		if rel, rerr := filepath.Rel(dir, path); rerr == nil {
			fmt.Printf("fabrik: wrote %s\n", rel)
		} else {
			fmt.Printf("fabrik: wrote %s\n", path)
		}
	}
	return filepath.Dir(out), nil
}

func writeGraphSidecars(mainDir string, graph *gen.Graph) ([]string, error) {
	data, err := json.MarshalIndent(graph, "", "  ")
	if err != nil {
		return nil, err
	}
	sidecars := []struct {
		name string
		data []byte
	}{
		{"fabrik.graph.json", append(data, '\n')},
		{"fabrik.graph.dot", []byte(graph.DOT())},
		{"fabrik.graph.mmd", []byte(graph.Mermaid())},
	}
	var written []string
	for _, s := range sidecars {
		path := filepath.Join(mainDir, s.name)
		if err := os.WriteFile(path, s.data, 0o644); err != nil { // #nosec G306 -- graph artifacts are intentionally readable
			return nil, err
		}
		written = append(written, path)
	}
	return written, nil
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
func wireCheck(dir string, opts engine.Options) error {
	res, out, err := generate(dir, opts)
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
	if !bytes.Equal(disk, res.Src) {
		fmt.Fprintf(os.Stderr, "fabrik: %s is stale; run fabrik wire\n", out)
		return errSilent
	}
	fmt.Printf("fabrik: main.gen.go up to date\n")
	return nil
}
