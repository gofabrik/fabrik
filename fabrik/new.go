package main

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
)

//go:embed templates
var templatesFS embed.FS

const starterRoot = "templates/starter"

func newCmd(args []string) error {
	module, args := extractFlag(args, "module")
	if len(args) < 1 {
		return fmt.Errorf("usage: fabrik new <project> [--module path]")
	}
	project := args[0]
	if strings.HasPrefix(project, "-") || len(args) > 1 {
		return fmt.Errorf("usage: fabrik new <project> [--module path]")
	}
	if module == "" {
		module = filepath.Base(project)
	}
	if strings.ContainsAny(module, " \t") {
		return fmt.Errorf("invalid module path %q", module)
	}
	if _, err := os.Stat(project); err == nil {
		return fmt.Errorf("%s already exists", project)
	}

	data := struct {
		Module    string
		EnvPrefix string
	}{Module: module, EnvPrefix: envPrefix(module)}

	err := fs.WalkDir(templatesFS, starterRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(starterRoot, path)
		if err != nil {
			return err
		}
		outPath := filepath.Join(project, strings.TrimSuffix(rel, ".tmpl"))

		content, err := templatesFS.ReadFile(path)
		if err != nil {
			return err
		}
		tmpl, err := template.New(rel).Parse(string(content))
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
			return err
		}
		out, err := os.Create(outPath)
		if err != nil {
			return err
		}
		if err := tmpl.Execute(out, data); err != nil {
			out.Close()
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
		fmt.Printf("  created  %s\n", outPath)
		return nil
	})
	if err != nil {
		return err
	}

	// The starter must resolve its router dependency before it can build.
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = project
	if out, terr := tidy.CombinedOutput(); terr != nil {
		fmt.Fprintf(os.Stderr, "%s", out)
		return fmt.Errorf("created %s, but its dependencies could not be resolved (offline?); run `go mod tidy` in %s and retry", project, project)
	}

	// Wire immediately and tidy again: the generated file imports modules
	// (config) that no hand-written source references, and the project
	// should build the moment new returns.
	abs, err := filepath.Abs(project)
	if err != nil {
		return err
	}
	if _, err := wire(abs); err != nil {
		return err
	}
	tidy = exec.Command("go", "mod", "tidy")
	tidy.Dir = project
	if out, terr := tidy.CombinedOutput(); terr != nil {
		fmt.Fprintf(os.Stderr, "%s", out)
		return fmt.Errorf("created and wired %s, but `go mod tidy` failed; fix and retry", project)
	}

	fmt.Printf("\nCreated %s. Next:\n  cd %s\n  fabrik run\n", project, project)
	return nil
}

// envPrefix derives the scaffolded app's environment prefix from the last
// module path element: my-app -> MY_APP.
func envPrefix(module string) string {
	base := module
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	var b strings.Builder
	for _, r := range strings.ToUpper(base) {
		if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	p := b.String()
	if p == "" || p[0] >= '0' && p[0] <= '9' {
		p = "APP" + p
	}
	return p
}

// extractFlag removes "--name value" or "--name=value" from args.
func extractFlag(args []string, name string) (string, []string) {
	prefix := "--" + name
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == prefix && i+1 < len(args):
			return args[i+1], append(append([]string{}, args[:i]...), args[i+2:]...)
		case strings.HasPrefix(args[i], prefix+"="):
			return strings.TrimPrefix(args[i], prefix+"="), append(append([]string{}, args[:i]...), args[i+1:]...)
		}
	}
	return "", args
}
