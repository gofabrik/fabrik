// Package goversion asserts the repository declares a single Go version, sourced
// from go.work's go directive.
package goversion

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"
	"gopkg.in/yaml.v3"

	"github.com/gofabrik/fabrik/internal/tools/modset"
)

// missing marks a Finding where the go directive, file, or pin is absent
// rather than present with the wrong value.
const missing = "(missing)"

// Finding describes one location where the declared Go version disagrees with go.work.
type Finding struct {
	Path     string
	Found    string
	Expected string
}

func (f Finding) String() string {
	return fmt.Sprintf("%s: found %s, want %s", f.Path, f.Found, f.Expected)
}

// Check compares every go.mod, the starter template, the engine fixtures, and the
// workflow go-version pins against go.work's go directive. A missing go directive,
// template, or fixture section is itself a Finding, not an error, so one run lists
// every mismatch; a genuine read failure still aborts with an error.
func Check(cfg *modset.Config) ([]Finding, error) {
	wf, err := loadWork(cfg.Root)
	if err != nil {
		return nil, err
	}
	if wf.Go == nil {
		return nil, fmt.Errorf("go.work: missing go directive")
	}

	var findings []Finding
	for _, check := range []func() ([]Finding, error){
		func() ([]Finding, error) { return checkGoMods(cfg, wf.Go.Version) },
		func() ([]Finding, error) { return checkTemplate(cfg.Root, wf.Go.Version) },
		func() ([]Finding, error) { return checkFixtures(cfg.Root, wf.Go.Version) },
		func() ([]Finding, error) { return checkWorkflows(cfg.Root, wf) },
	} {
		f, err := check()
		if err != nil {
			return nil, err
		}
		findings = append(findings, f...)
	}

	sort.Slice(findings, func(i, j int) bool { return findings[i].Path < findings[j].Path })
	return findings, nil
}

func loadWork(root string) (*modfile.WorkFile, error) {
	path := filepath.Join(root, "go.work")
	// #nosec G304 -- reads a build/workspace path
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return modfile.ParseWork(path, data, nil)
}

// checkGoMods compares every workspace member's go.mod, plus internal/tools' own
// go.mod (outside the workspace), against go.work's go directive. A go.mod that
// parses but has no go directive is a Finding; a go.mod that cannot be read or
// parsed at all is an error - that is a broken workspace member, not drift.
func checkGoMods(cfg *modset.Config, want string) ([]Finding, error) {
	paths := make([]string, 0, len(cfg.Modules)+1)
	for _, dir := range cfg.Modules {
		paths = append(paths, filepath.Join(dir, "go.mod"))
	}
	paths = append(paths, filepath.Join(cfg.Root, "internal", "tools", "go.mod"))
	sort.Strings(paths)

	var findings []Finding
	for _, p := range paths {
		// #nosec G304 -- reads a build/workspace path
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		mf, err := modfile.Parse(p, data, nil)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", p, err)
		}
		found := missing
		if mf.Go != nil {
			found = mf.Go.Version
		}
		if found != want {
			findings = append(findings, Finding{Path: relPath(cfg.Root, p), Found: found, Expected: want})
		}
	}
	return findings, nil
}

var goDirectiveRE = regexp.MustCompile(`(?m)^go (\S+)\s*$`)

// checkTemplate scans the starter's go.mod.tmpl as text: it is not valid go.mod
// syntax once the templated require block is included. A deleted template or a
// template with no go line is a Finding, not an error - both are legitimate
// drift the gate should report alongside every other mismatch.
func checkTemplate(root, want string) ([]Finding, error) {
	path := filepath.Join(root, "fabrik", "templates", "starter", "go.mod.tmpl")
	// #nosec G304 -- reads a build/workspace path
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []Finding{{Path: relPath(root, path), Found: missing, Expected: want}}, nil
		}
		return nil, err
	}
	found := missing
	if m := goDirectiveRE.FindStringSubmatch(string(data)); m != nil {
		found = m[1]
	}
	if found == want {
		return nil, nil
	}
	return []Finding{{Path: relPath(root, path), Found: found, Expected: want}}, nil
}

var txtarHeaderRE = regexp.MustCompile(`^-- (\S.*?) --$`)

// checkFixtures scans each engine testdata fixture's embedded "-- go.mod --"
// section as text, matching the txtar file format directly rather than pulling
// in the txtar dependency for a single-line scan. A fixture with no such section,
// or a section with no go line, is a Finding, not an error; a fixture that cannot
// be read at all is a real IO error.
func checkFixtures(root, want string) ([]Finding, error) {
	dir := filepath.Join(root, "fabrik", "internal", "engine", "testdata")
	matches, err := filepath.Glob(filepath.Join(dir, "*.txt"))
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)

	var findings []Finding
	for _, p := range matches {
		// #nosec G304 -- reads a build/workspace path
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		found := missing
		if section, ok := goModSection(string(data)); ok {
			if m := goDirectiveRE.FindStringSubmatch(section); m != nil {
				found = m[1]
			}
		}
		if found != want {
			findings = append(findings, Finding{Path: relPath(root, p), Found: found, Expected: want})
		}
	}
	return findings, nil
}

// goModSection extracts a txtar fixture's "-- go.mod --" file section.
func goModSection(data string) (string, bool) {
	inSection := false
	var block []string
	for _, line := range strings.Split(data, "\n") {
		if m := txtarHeaderRE.FindStringSubmatch(line); m != nil {
			if inSection {
				break
			}
			if m[1] == "go.mod" {
				inSection = true
			}
			continue
		}
		if inSection {
			block = append(block, line)
		}
	}
	if !inSection {
		return "", false
	}
	return strings.Join(block, "\n"), true
}

var majorMinorRE = regexp.MustCompile(`^(\d+\.\d+)`)

const setupGoUsesPrefix = "actions/setup-go@"

// checkWorkflows compares every actions/setup-go step's go-version pin against
// go.work, across both *.yml and *.yaml workflow files. Each file is parsed as
// YAML (not scanned as text), so comments and unrelated keys such as env cannot
// be mistaken for a step or its pin. A setup-go step whose with.go-version is
// absent or empty is itself a Finding. With a toolchain directive present,
// pins match it (either spelling); otherwise pins match the go directive's
// major.minor (a patched go directive like "1.27.0" accepts "1.27").
func checkWorkflows(root string, wf *modfile.WorkFile) ([]Finding, error) {
	dir := filepath.Join(root, ".github", "workflows")
	var matches []string
	for _, pattern := range []string{"*.yml", "*.yaml"} {
		m, err := filepath.Glob(filepath.Join(dir, pattern))
		if err != nil {
			return nil, err
		}
		matches = append(matches, m...)
	}
	sort.Strings(matches)

	hasToolchain := wf.Toolchain != nil
	var expected string
	var err error
	if hasToolchain {
		expected, err = canonicalVersion(wf.Toolchain.Name)
		if err != nil {
			return nil, fmt.Errorf("go.work toolchain %q: %w", wf.Toolchain.Name, err)
		}
	} else {
		expected, err = majorMinor(wf.Go.Version)
		if err != nil {
			return nil, fmt.Errorf("go.work go directive %q: %w", wf.Go.Version, err)
		}
	}

	var findings []Finding
	for _, p := range matches {
		// #nosec G304 -- reads a build/workspace path
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("parse %s: %w", p, err)
		}
		for _, pin := range setupGoPins(&doc) {
			if !workflowPinMatches(pin.value, expected, hasToolchain) {
				findings = append(findings, Finding{
					Path:     fmt.Sprintf("%s:%d", relPath(root, p), pin.line),
					Found:    pin.value,
					Expected: expected,
				})
			}
		}
	}
	return findings, nil
}

// setupGoPin is one actions/setup-go step's resolved go-version pin.
type setupGoPin struct {
	value string
	line  int
}

// setupGoPins walks a parsed workflow document's jobs[*].steps[*] and returns
// the go-version pin for every step whose uses starts with the setup-go
// action. A step with no with.go-version key, or an empty one, reports
// missing at the uses line; yaml.v3 already strips comments and separates
// env from with, so neither can be mistaken for a real pin.
func setupGoPins(doc *yaml.Node) []setupGoPin {
	if len(doc.Content) == 0 {
		return nil
	}
	jobs := mapValue(doc.Content[0], "jobs")
	if jobs == nil || jobs.Kind != yaml.MappingNode {
		return nil
	}
	var pins []setupGoPin
	for i := 1; i < len(jobs.Content); i += 2 {
		steps := mapValue(jobs.Content[i], "steps")
		if steps == nil || steps.Kind != yaml.SequenceNode {
			continue
		}
		for _, step := range steps.Content {
			uses := mapValue(step, "uses")
			if uses == nil || uses.Kind != yaml.ScalarNode || !strings.HasPrefix(uses.Value, setupGoUsesPrefix) {
				continue
			}
			pin := setupGoPin{value: missing, line: uses.Line}
			if version := mapValue(mapValue(step, "with"), "go-version"); version != nil {
				pin.line = version.Line
				if version.Value != "" {
					pin.value = version.Value
				}
			}
			pins = append(pins, pin)
		}
	}
	return pins
}

// mapValue returns the value node for key in a YAML mapping node, or nil if n
// is not a mapping or has no such key.
func mapValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

func workflowPinMatches(found, expected string, hasToolchain bool) bool {
	if found == missing {
		return false
	}
	if found == expected {
		return true
	}
	if !hasToolchain {
		return false
	}
	canon, err := canonicalVersion(found)
	return err == nil && canon == expected
}

// majorMinor reduces a go directive value ("1.27" or "1.27.0") to "major.minor".
func majorMinor(v string) (string, error) {
	m := majorMinorRE.FindStringSubmatch(v)
	if m == nil {
		return "", fmt.Errorf("unrecognized Go version %q", v)
	}
	return m[1], nil
}

var (
	hyphenVersionRE = regexp.MustCompile(`^(\d+)\.(\d+)(?:\.(\d+))?-(rc|beta)\.(\d+)$`)
	tightVersionRE  = regexp.MustCompile(`^(\d+)\.(\d+)(?:\.(\d+))?(rc|beta)(\d+)$`)
	bareVersionRE   = regexp.MustCompile(`^(\d+)\.(\d+)(?:\.(\d+))?$`)
)

// canonicalVersion normalizes a Go version spelling - toolchain style ("go1.27rc2")
// or setup-go style ("1.27.0-rc.2") - to "major.minor.patch[-pre.N]" so the two
// spellings compare equal.
func canonicalVersion(s string) (string, error) {
	s = strings.TrimPrefix(s, "go")
	if m := hyphenVersionRE.FindStringSubmatch(s); m != nil {
		return assembleVersion(m[1], m[2], m[3], m[4], m[5]), nil
	}
	if m := tightVersionRE.FindStringSubmatch(s); m != nil {
		return assembleVersion(m[1], m[2], m[3], m[4], m[5]), nil
	}
	if m := bareVersionRE.FindStringSubmatch(s); m != nil {
		return assembleVersion(m[1], m[2], m[3], "", ""), nil
	}
	return "", fmt.Errorf("unrecognized Go version %q", s)
}

func assembleVersion(major, minor, patch, pre, preNum string) string {
	if patch == "" {
		patch = "0"
	}
	v := major + "." + minor + "." + patch
	if pre != "" {
		v += "-" + pre + "." + preNum
	}
	return v
}

func relPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}
