// Package goversion checks repository Go version declarations against go.work.
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

const missing = "(missing)"

const none = "(none)"

// Finding describes one location where the declared Go version disagrees with go.work.
type Finding struct {
	Path     string
	Found    string
	Expected string
}

func (f Finding) String() string {
	return fmt.Sprintf("%s: found %s, want %s", f.Path, f.Found, f.Expected)
}

// Check reports missing or inconsistent Go and toolchain declarations across the repository.
func Check(cfg *modset.Config) ([]Finding, error) {
	wf, err := loadWork(cfg.Root)
	if err != nil {
		return nil, err
	}
	if wf.Go == nil {
		return nil, fmt.Errorf("go.work: missing go directive")
	}

	var wantToolchain string
	if wf.Toolchain != nil {
		wantToolchain = wf.Toolchain.Name
	}

	var findings []Finding
	for _, check := range []func() ([]Finding, error){
		func() ([]Finding, error) { return checkGoMods(cfg, wf.Go.Version, wantToolchain) },
		func() ([]Finding, error) { return checkTemplate(cfg.Root, wf.Go.Version, wantToolchain) },
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

// Unreadable or invalid go.mod files are errors; missing directives are findings.
func checkGoMods(cfg *modset.Config, want, wantToolchain string) ([]Finding, error) {
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
		foundToolchain := ""
		if mf.Toolchain != nil {
			foundToolchain = mf.Toolchain.Name
		}
		if f, e, ok := toolchainFinding(foundToolchain, wantToolchain); ok {
			findings = append(findings, Finding{Path: relPath(cfg.Root, p), Found: f, Expected: e})
		}
	}
	return findings, nil
}

func toolchainFinding(found, want string) (string, string, bool) {
	switch {
	case found == want:
		return "", "", false
	case want == "":
		return found, none, true
	case found == "":
		return missing, want, true
	default:
		return found, want, true
	}
}

var (
	goDirectiveRE        = regexp.MustCompile(`(?m)^go (\S+)\s*$`)
	toolchainDirectiveRE = regexp.MustCompile(`(?m)^toolchain (\S+)\s*$`)
)

// Scan the template as text because its rendered require block is not valid go.mod syntax.
func checkTemplate(root, want, wantToolchain string) ([]Finding, error) {
	path := filepath.Join(root, "fabrik", "templates", "starter", "go.mod.tmpl")
	rel := relPath(root, path)
	// #nosec G304 -- reads a build/workspace path
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []Finding{{Path: rel, Found: missing, Expected: want}}, nil
		}
		return nil, err
	}
	var findings []Finding
	found := missing
	if m := goDirectiveRE.FindStringSubmatch(string(data)); m != nil {
		found = m[1]
	}
	if found != want {
		findings = append(findings, Finding{Path: rel, Found: found, Expected: want})
	}
	foundToolchain := ""
	if m := toolchainDirectiveRE.FindStringSubmatch(string(data)); m != nil {
		foundToolchain = m[1]
	}
	if f, e, ok := toolchainFinding(foundToolchain, wantToolchain); ok {
		findings = append(findings, Finding{Path: rel, Found: f, Expected: e})
	}
	return findings, nil
}

var txtarHeaderRE = regexp.MustCompile(`^-- (\S.*?) --$`)

// Fixtures omit toolchain directives because the harness supplies GOTOOLCHAIN.
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
		foundToolchain := ""
		if section, ok := goModSection(string(data)); ok {
			if m := goDirectiveRE.FindStringSubmatch(section); m != nil {
				found = m[1]
			}
			if m := toolchainDirectiveRE.FindStringSubmatch(section); m != nil {
				foundToolchain = m[1]
			}
		}
		if found != want {
			findings = append(findings, Finding{Path: relPath(root, p), Found: found, Expected: want})
		}
		if f, e, ok := toolchainFinding(foundToolchain, ""); ok {
			findings = append(findings, Finding{Path: relPath(root, p), Found: f, Expected: e})
		}
	}
	return findings, nil
}

func goModSection(data string) (string, bool) {
	inSection := false
	var block []string
	for line := range strings.SplitSeq(data, "\n") {
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

// Workflow pins match the toolchain directive, or the go directive's major.minor without one.
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

type setupGoPin struct {
	value string
	line  int
}

// Missing or empty go-version pins are reported at the setup-go uses line.
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

// canonicalVersion normalizes toolchain and setup-go spellings for comparison.
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
