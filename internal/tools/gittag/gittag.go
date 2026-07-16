// Package gittag creates and atomically pushes unsigned path-prefixed release tags.
package gittag

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gofabrik/fabrik/internal/tools/modset"
)

// Plan returns one path-prefixed tag name per published module.
func Plan(cfg *modset.Config) ([]string, error) {
	tags := make([]string, 0, len(cfg.Published))
	for p := range cfg.Published {
		dir, ok := cfg.Modules[p]
		if !ok {
			return nil, fmt.Errorf("published module %s is not in the workspace", p)
		}
		rel, err := relSlash(cfg.Root, dir)
		if err != nil {
			return nil, err
		}
		tags = append(tags, rel+"/"+cfg.Version)
	}
	sort.Strings(tags)
	return tags, nil
}

// Create applies all planned tags to commit and optionally pushes them atomically, rejecting version mismatches and conflicting tags.
func Create(cfg *modset.Config, commit, remote string, push bool) ([]string, error) {
	want, err := resolveCommit(cfg.Root, commit)
	if err != nil {
		return nil, err
	}
	if v, err := versionAtCommit(cfg.Root, commit); err != nil {
		return nil, err
	} else if v != cfg.Version {
		return nil, fmt.Errorf("commit %s declares version %q, but the release is %q; tag the release commit", commit, v, cfg.Version)
	}

	tags, err := Plan(cfg)
	if err != nil {
		return nil, err
	}

	for _, t := range tags {
		at, ok := tagCommit(cfg.Root, t)
		if ok {
			if at != want {
				return nil, fmt.Errorf("tag %s already exists at %s, not %s; pick a new version", t, at, want)
			}
			continue
		}
		if out, err := git(cfg.Root, "tag", "-a", t, "-m", "Release "+cfg.Version, commit); err != nil {
			return nil, fmt.Errorf("create tag %s: %v\n%s", t, err, out)
		}
	}

	if push {
		args := append([]string{"push", "--atomic", remote}, tags...)
		if out, err := git(cfg.Root, args...); err != nil {
			return tags, fmt.Errorf("push tags: %v\n%s", err, out)
		}
	}
	return tags, nil
}

func tagCommit(dir, tag string) (string, bool) {
	out, err := git(dir, "rev-parse", "-q", "--verify", "refs/tags/"+tag+"^{commit}")
	if err != nil {
		return "", false
	}
	return out, true
}

func resolveCommit(dir, commit string) (string, error) {
	out, err := git(dir, "rev-parse", "--verify", commit+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve commit %s: not found locally; fetch it before tagging: %v\n%s", commit, err, out)
	}
	return out, nil
}

func versionAtCommit(dir, commit string) (string, error) {
	out, err := git(dir, "show", commit+":versions.yaml")
	if err != nil {
		return "", fmt.Errorf("versions.yaml not found at %s; use the release commit: %v\n%s", commit, err, out)
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "version:" {
			return f[1], nil
		}
	}
	return "", fmt.Errorf("no version found in versions.yaml at %s", commit)
}

func relSlash(root, dir string) (string, error) {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}
