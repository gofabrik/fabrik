package main

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
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
	if module == "" {
		module = project
	}
	if _, err := os.Stat(project); err == nil {
		return fmt.Errorf("%s already exists", project)
	}

	data := struct{ Module string }{Module: module}

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
		defer out.Close()
		if err := tmpl.Execute(out, data); err != nil {
			return err
		}
		fmt.Printf("  created  %s\n", outPath)
		return nil
	})
	if err != nil {
		return err
	}

	fmt.Printf("\nCreated %s. Next:\n  cd %s\n  fabrik run\n", project, project)
	return nil
}

// extractFlag pulls "--name value" or "--name=value" out of args regardless of
// position, returning the value and the remaining args.
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
