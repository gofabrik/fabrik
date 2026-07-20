// Package converge stabilizes module sums against pre-release archives and verifies readonly builds.
package converge

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gofabrik/fabrik/internal/tools/candidateproxy"
	"github.com/gofabrik/fabrik/internal/tools/modset"
)

// Run tidies to a fixed point within maxIters, then verifies every module.
func Run(cfg *modset.Config, maxIters int) (int, error) {
	dirs := sortedDirs(cfg)
	for i := 1; i <= maxIters; i++ {
		before, err := snapshot(dirs)
		if err != nil {
			return 0, err
		}
		if err := tidyPass(cfg, dirs); err != nil {
			return 0, err
		}
		after, err := snapshot(dirs)
		if err != nil {
			return 0, err
		}
		if before == after {
			if err := Verify(cfg); err != nil {
				return i, err
			}
			return i, nil
		}
	}
	return 0, fmt.Errorf("did not converge in %d iterations", maxIters)
}

// Verify builds every workspace module readonly against a worktree candidate proxy.
func Verify(cfg *modset.Config) error {
	proxy, modcache, cleanup, err := freshProxy(cfg)
	if err != nil {
		return err
	}
	defer cleanup()
	env := candidateproxy.Env(proxy, modcache)
	for _, dir := range sortedDirs(cfg) {
		cmd := exec.Command("go", "build", "-mod=readonly", "./...")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("readonly build failed in %s: %w\n%s", dir, err, out)
		}
	}
	return nil
}

func tidyPass(cfg *modset.Config, dirs []string) error {
	// Materialize the proxy before changing manifests used to build its archives.
	proxy, modcache, cleanup, err := freshProxy(cfg)
	if err != nil {
		return err
	}
	defer cleanup()
	env := candidateproxy.Env(proxy, modcache)
	for _, dir := range dirs {
		// Drop stale same-version sums so tidy accepts the materialized archive.
		if err := stripIntraSums(cfg, dir); err != nil {
			return err
		}
		cmd := exec.Command("go", "mod", "tidy")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("go mod tidy failed in %s: %w\n%s", dir, err, out)
		}
	}
	return nil
}

// stripIntraSums removes target-version sums for published modules.
func stripIntraSums(cfg *modset.Config, dir string) error {
	p := filepath.Join(dir, "go.sum")
	// #nosec G304 -- reads a build/workspace path
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	lines := strings.Split(string(data), "\n")
	kept := lines[:0]
	for _, line := range lines {
		f := strings.Fields(line)
		if len(f) >= 2 && cfg.Published[f[0]] {
			ver := strings.TrimSuffix(f[1], "/go.mod")
			if ver == cfg.Version {
				continue
			}
		}
		kept = append(kept, line)
	}
	return os.WriteFile(p, []byte(strings.Join(kept, "\n")), 0o600) // #nosec G703 -- path derived from the trusted repo/module layout
}

// A fresh cache prevents an older archive at the same version from shadowing rebuilt content.
func freshProxy(cfg *modset.Config) (proxy, modcache string, cleanup func(), err error) {
	proxy, err = os.MkdirTemp("", "candidate-proxy-")
	if err != nil {
		return "", "", nil, err
	}
	modcache, err = os.MkdirTemp("", "candidate-modcache-")
	if err != nil {
		// #nosec G104 -- best-effort cleanup after temporary cache creation failed
		//nolint:errcheck // cleanup after temporary cache creation failed
		os.RemoveAll(proxy)
		return "", "", nil, err
	}
	cleanup = func() {
		makeWritable(modcache)
		// #nosec G104 -- best-effort cleanup of a temporary candidate proxy
		//nolint:errcheck // cleanup of a temporary candidate proxy
		os.RemoveAll(proxy)
		// #nosec G104 -- best-effort cleanup of a temporary module cache
		//nolint:errcheck // cleanup of a temporary module cache
		os.RemoveAll(modcache)
	}
	if err := candidateproxy.BuildWorktree(cfg, proxy); err != nil {
		cleanup()
		return "", "", nil, err
	}
	return proxy, modcache, cleanup, nil
}

func sortedDirs(cfg *modset.Config) []string {
	dirs := make([]string, 0, len(cfg.Modules))
	for _, dir := range cfg.Modules {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs
}

// snapshot hashes module manifests to detect a fixed point.
func snapshot(dirs []string) ([32]byte, error) {
	h := sha256.New()
	for _, dir := range dirs {
		for _, name := range []string{"go.mod", "go.sum"} {
			// #nosec G304 -- reads a build/workspace path
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return [32]byte{}, err
			}
			//nolint:errcheck // hash.Hash writes never return an error
			fmt.Fprintf(h, "%s\x00%d\x00", filepath.Join(dir, name), len(data))
			h.Write(data)
		}
	}
	return [32]byte(h.Sum(nil)), nil
}

// Go's module cache is read-only by default.
func makeWritable(root string) {
	// #nosec G104 -- best-effort cleanup of a temp cache
	//nolint:errcheck // best-effort cleanup of a temp cache
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil {
			mode := os.FileMode(0o600)
			if d.IsDir() {
				mode = 0o700
			}
			// #nosec G104 -- best-effort cleanup of a temp cache
			//nolint:errcheck // best-effort cleanup of a temp cache
			os.Chmod(p, mode) // #nosec G122 -- trusted build path
		}
		return nil
	})
}
