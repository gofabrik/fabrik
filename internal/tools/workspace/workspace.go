// Package workspace keeps go.work replacements aligned with the release module set.
package workspace

import (
	"os"
	"path/filepath"
	"sort"

	"golang.org/x/mod/modfile"

	"github.com/gofabrik/fabrik/internal/tools/modset"
)

// Sync rewrites go.work replacements to match versions.yaml.
func Sync(cfg *modset.Config) (bool, error) {
	workPath := filepath.Join(cfg.Root, "go.work")
	want, data, err := generate(cfg)
	if err != nil {
		return false, err
	}
	if string(want) == string(data) {
		return false, nil
	}
	return true, os.WriteFile(workPath, want, 0o644)
}

// Check reports whether go.work replacements differ from versions.yaml.
func Check(cfg *modset.Config) (drift bool, err error) {
	want, data, err := generate(cfg)
	if err != nil {
		return false, err
	}
	return string(want) != string(data), nil
}

func generate(cfg *modset.Config) (want, current []byte, err error) {
	workPath := filepath.Join(cfg.Root, "go.work")
	data, err := os.ReadFile(workPath)
	if err != nil {
		return nil, nil, err
	}
	wf, err := modfile.ParseWork(workPath, data, nil)
	if err != nil {
		return nil, nil, err
	}

	type key struct{ path, version string }
	var drop []key
	for _, r := range wf.Replace {
		if cfg.Published[r.Old.Path] || cfg.Modules[r.Old.Path] != "" {
			drop = append(drop, key{r.Old.Path, r.Old.Version})
		}
	}
	for _, k := range drop {
		if err := wf.DropReplace(k.path, k.version); err != nil {
			return nil, nil, err
		}
	}

	// A single block avoids formatter-inserted blank lines between replacements.
	paths := make([]string, 0, len(cfg.Published))
	for p := range cfg.Published {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	block := &modfile.LineBlock{Token: []string{"replace"}}
	for _, p := range paths {
		dir, ok := cfg.Modules[p]
		if !ok {
			continue
		}
		rel, err := filepath.Rel(cfg.Root, dir)
		if err != nil {
			return nil, nil, err
		}
		block.Line = append(block.Line, &modfile.Line{
			InBlock: true,
			Token:   []string{p, cfg.Version, "=>", "./" + filepath.ToSlash(rel)},
		})
	}
	if len(block.Line) > 0 {
		wf.Syntax.Stmt = append(wf.Syntax.Stmt, block)
	}

	wf.Cleanup()
	return modfile.Format(wf.Syntax), data, nil
}
