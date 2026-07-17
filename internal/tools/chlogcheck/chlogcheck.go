// Package chlogcheck validates chloggen changelog fragments. It mirrors chloggen's
// own checks (valid change_type, known component, non-empty note, valid change_logs
// keys) but deliberately does not require issue links: fabrik changelog fragments do
// not reference issues, and chloggen's validate has no switch to relax that rule.
package chlogcheck

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

var changeTypes = []string{"breaking", "deprecation", "new_component", "enhancement", "bug_fix"}

type config struct {
	ChangeLogs        map[string]string `yaml:"change_logs"`
	DefaultChangeLogs []string          `yaml:"default_change_logs"`
	EntriesDir        string            `yaml:"entries_dir"`
	TemplateYAML      string            `yaml:"template_yaml"`
	Components        []string          `yaml:"components"`
}

// entry is a changelog fragment. Issues is intentionally omitted: it is neither
// required nor inspected.
type entry struct {
	ChangeLogs []string `yaml:"change_logs"`
	ChangeType string   `yaml:"change_type"`
	Component  string   `yaml:"component"`
	Note       string   `yaml:"note"`
}

// Validate reads the chloggen config and checks every fragment in its entries
// directory, returning a joined error naming each bad fragment, or nil if all pass.
func Validate(configPath string) error {
	cfgBytes, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	var cfg config
	if err := yaml.Unmarshal(cfgBytes, &cfg); err != nil {
		return fmt.Errorf("%s: %w", configPath, err)
	}

	// Fragments live alongside the config by default (chloggen's <root>/.chloggen).
	entriesDir := filepath.Dir(configPath)
	if cfg.EntriesDir != "" {
		if filepath.IsAbs(cfg.EntriesDir) {
			entriesDir = cfg.EntriesDir
		} else {
			entriesDir = filepath.Join(filepath.Dir(filepath.Dir(configPath)), cfg.EntriesDir)
		}
	}
	if _, err := os.Stat(entriesDir); err != nil {
		return err
	}

	skip := map[string]bool{filepath.Base(configPath): true, "TEMPLATE.yaml": true}
	if cfg.TemplateYAML != "" {
		skip[filepath.Base(cfg.TemplateYAML)] = true
	}

	var files []string
	for _, pat := range []string{"*.yaml", "*.yml"} {
		matches, err := filepath.Glob(filepath.Join(entriesDir, pat))
		if err != nil {
			return err
		}
		files = append(files, matches...)
	}
	slices.Sort(files)

	changelogRequired := len(cfg.DefaultChangeLogs) == 0
	validChangeLogs := make([]string, 0, len(cfg.ChangeLogs))
	for key := range cfg.ChangeLogs {
		validChangeLogs = append(validChangeLogs, key)
	}
	slices.Sort(validChangeLogs)

	var errs error
	for _, file := range files {
		if skip[filepath.Base(file)] {
			continue
		}
		body, err := os.ReadFile(file)
		if err != nil {
			errs = errors.Join(errs, err)
			continue
		}
		var e entry
		if err := yaml.Unmarshal(body, &e); err != nil {
			errs = errors.Join(errs, fmt.Errorf("%s: %w", filepath.Base(file), err))
			continue
		}
		if err := validateEntry(e, cfg.Components, validChangeLogs, changelogRequired); err != nil {
			errs = errors.Join(errs, fmt.Errorf("%s: %w", filepath.Base(file), err))
		}
	}
	return errs
}

func validateEntry(e entry, components, validChangeLogs []string, changelogRequired bool) error {
	var errs error
	if changelogRequired && len(e.ChangeLogs) == 0 {
		errs = errors.Join(errs, errors.New("specify one or more 'change_logs'"))
	}
	for _, cl := range e.ChangeLogs {
		if !slices.Contains(validChangeLogs, cl) {
			errs = errors.Join(errs, fmt.Errorf("'%s' is not a valid value in 'change_logs'. Specify one of %v", cl, validChangeLogs))
		}
	}
	if !slices.Contains(changeTypes, e.ChangeType) {
		errs = errors.Join(errs, fmt.Errorf("'%s' is not a valid 'change_type'. Specify one of %v", e.ChangeType, changeTypes))
	}
	if strings.TrimSpace(e.Component) == "" {
		errs = errors.Join(errs, errors.New("specify a 'component'"))
	} else if len(components) > 0 && !slices.Contains(components, e.Component) {
		errs = errors.Join(errs, fmt.Errorf("'%s' is not a valid 'component'. It must be one of %v", e.Component, components))
	}
	if strings.TrimSpace(e.Note) == "" {
		errs = errors.Join(errs, errors.New("specify a 'note'"))
	}
	return errs
}
