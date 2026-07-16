// Package modset loads the repository's lockstep release configuration and workspace modules.
package modset

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"golang.org/x/mod/modfile"
	"gopkg.in/yaml.v3"
)

var versionLine = regexp.MustCompile(`(?m)^(\s+version:\s+)\S+`)

// SetVersion updates versions.yaml while preserving surrounding comments and formatting.
func SetVersion(root, version string) error {
	path := filepath.Join(root, "versions.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !versionLine.Match(data) {
		return fmt.Errorf("no module-set version line found in %s", path)
	}
	out := versionLine.ReplaceAll(data, []byte("${1}"+version))
	return os.WriteFile(path, out, 0o644)
}

// Config combines release metadata with resolved workspace modules.
type Config struct {
	Root      string            // repo root: the dir holding versions.yaml and go.work
	Version   string            // lockstep version from versions.yaml, e.g. "v0.1.0"
	Published map[string]bool   // module paths in the release module-set
	Excluded  map[string]bool   // module paths under excluded-modules
	Modules   map[string]string // every go.work module path -> absolute local dir
}

type versionsFile struct {
	ModuleSets map[string]struct {
		Version string   `yaml:"version"`
		Modules []string `yaml:"modules"`
	} `yaml:"module-sets"`
	ExcludedModules []string `yaml:"excluded-modules"`
}

// Load finds and parses the repository's single lockstep module set.
func Load(start string) (*Config, error) {
	root, err := findRoot(start)
	if err != nil {
		return nil, err
	}

	vf, err := parseVersions(filepath.Join(root, "versions.yaml"))
	if err != nil {
		return nil, err
	}
	if len(vf.ModuleSets) != 1 {
		return nil, fmt.Errorf("versions.yaml: want exactly one module-set, found %d", len(vf.ModuleSets))
	}
	cfg := &Config{
		Root:      root,
		Published: map[string]bool{},
		Excluded:  map[string]bool{},
		Modules:   map[string]string{},
	}
	for _, set := range vf.ModuleSets {
		cfg.Version = set.Version
		for _, m := range set.Modules {
			cfg.Published[m] = true
		}
	}
	for _, m := range vf.ExcludedModules {
		cfg.Excluded[m] = true
	}

	if err := cfg.loadWorkspace(); err != nil {
		return nil, err
	}
	// Published modules must be visible to workspace build and release checks.
	for m := range cfg.Published {
		if _, ok := cfg.Modules[m]; !ok {
			return nil, fmt.Errorf("published module %s is in versions.yaml but missing from go.work; add it with `go work use`", m)
		}
	}
	return cfg, nil
}

func (c *Config) loadWorkspace() error {
	workPath := filepath.Join(c.Root, "go.work")
	data, err := os.ReadFile(workPath)
	if err != nil {
		return err
	}
	wf, err := modfile.ParseWork(workPath, data, nil)
	if err != nil {
		return fmt.Errorf("parse go.work: %w", err)
	}
	for _, u := range wf.Use {
		dir := filepath.Join(c.Root, filepath.FromSlash(u.Path))
		path, err := modulePath(dir)
		if err != nil {
			return err
		}
		c.Modules[path] = dir
	}
	return nil
}

func modulePath(dir string) (string, error) {
	gomod := filepath.Join(dir, "go.mod")
	data, err := os.ReadFile(gomod)
	if err != nil {
		return "", err
	}
	return modfile.ModulePath(data), nil
}

func parseVersions(path string) (*versionsFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var vf versionsFile
	if err := yaml.Unmarshal(data, &vf); err != nil {
		return nil, fmt.Errorf("parse versions.yaml: %w", err)
	}
	return &vf, nil
}

func findRoot(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "versions.yaml")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("versions.yaml not found at or above %s", start)
		}
		dir = parent
	}
}
