package main

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"text/template"
)

// all: is required - the starter carries the app's own _layout.html
// and _-prefixed template partials, which a plain pattern would drop.
//
//go:embed all:templates
var templatesFS embed.FS

const starterRoot = "templates/starter"

// starterFabrikModules includes dependencies introduced by code generation.
var starterFabrikModules = []string{
	"github.com/gofabrik/fabrik/assetmapper",
	"github.com/gofabrik/fabrik/config",
	"github.com/gofabrik/fabrik/router",
	"github.com/gofabrik/fabrik/templates",
	"github.com/gofabrik/fabrik/web",
}

var scaffoldVersion = func() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	switch info.Main.Version {
	case "", "(devel)":
		return ""
	default:
		return info.Main.Version
	}
}

type scaffoldVars struct {
	Module        string
	EnvPrefix     string
	FabrikVersion string
	FabrikModules []string
}

func scaffoldData(module, version string) scaffoldVars {
	v := scaffoldVars{Module: module, EnvPrefix: envPrefix(module), FabrikVersion: version}
	if version != "" {
		v.FabrikModules = starterFabrikModules
	}
	return v
}

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

	version := scaffoldVersion()
	data := scaffoldData(module, version)

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
		if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
			return err
		}
		// Only .tmpl files are templates of the scaffold; everything else
		// (assets, the app's own HTML templates) copies verbatim.
		if !strings.HasSuffix(path, ".tmpl") {
			if err := os.WriteFile(outPath, content, 0o644); err != nil {
				return err
			}
			fmt.Printf("  created  %s\n", outPath)
			return nil
		}
		tmpl, err := template.New(rel).Parse(string(content))
		if err != nil {
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

	// Resolve starter dependencies before wiring.
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = project
	if out, terr := tidy.CombinedOutput(); terr != nil {
		fmt.Fprintf(os.Stderr, "%s", out)
		return fmt.Errorf("created %s, but its dependencies could not be resolved (offline?); run `go mod tidy` in %s and retry", project, project)
	}

	// Generated imports may add modules no handwritten source references.
	abs, err := filepath.Abs(project)
	if err != nil {
		return err
	}
	if _, err := wire(abs); err != nil {
		return err
	}
	// Generated-only dependencies must stay at the CLI's version after the final tidy.
	if version != "" {
		if err := repinFabrik(project, version); err != nil {
			return err
		}
	}
	tidy = exec.Command("go", "mod", "tidy")
	tidy.Dir = project
	if out, terr := tidy.CombinedOutput(); terr != nil {
		fmt.Fprintf(os.Stderr, "%s", out)
		return fmt.Errorf("created and wired %s, but `go mod tidy` failed; fix and retry", project)
	}

	mode := "resolving fabrik from the workspace"
	if version != "" {
		mode = "pinned fabrik " + version
	}
	fmt.Printf("\nCreated %s (%s). Next:\n  cd %s\n  fabrik run\n", project, mode, project)
	return nil
}

func repinFabrik(dir, version string) error {
	for _, m := range starterFabrikModules {
		edit := exec.Command("go", "mod", "edit", "-require="+m+"@"+version)
		edit.Dir = dir
		if out, err := edit.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "%s", out)
			return fmt.Errorf("re-pin fabrik requires in %s: %w", dir, err)
		}
	}
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
