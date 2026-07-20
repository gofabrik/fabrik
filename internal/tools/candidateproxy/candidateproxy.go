// Package candidateproxy builds local Go module proxies for pre-release verification.
package candidateproxy

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"

	"golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"

	"github.com/gofabrik/fabrik/internal/tools/modset"
)

// The .info timestamp does not affect module hashes, so a constant keeps output deterministic.
const fixedInfoTime = "2000-01-01T00:00:00Z"

// Build packages every published module from revision with canonical VCS archive semantics.
func Build(cfg *modset.Config, outDir, revision string) error {
	for path := range cfg.Published {
		dir, ok := cfg.Modules[path]
		if !ok {
			return fmt.Errorf("published module %s is not in the workspace", path)
		}
		subdir, err := filepath.Rel(cfg.Root, dir)
		if err != nil {
			return err
		}
		if err := writeModule(outDir, cfg.Root, revision, path, cfg.Version, filepath.ToSlash(subdir)); err != nil {
			return fmt.Errorf("package %s: %w", path, err)
		}
	}
	return nil
}

// BuildWorktree packages uncommitted files while preserving canonical root-LICENSE behavior.
func BuildWorktree(cfg *modset.Config, outDir string) error {
	license, err := os.ReadFile(filepath.Join(cfg.Root, "LICENSE"))
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		license = nil
	}
	for path := range cfg.Published {
		dir, ok := cfg.Modules[path]
		if !ok {
			return fmt.Errorf("published module %s is not in the workspace", path)
		}
		if err := writeModuleWorktree(outDir, path, cfg.Version, dir, license); err != nil {
			return fmt.Errorf("package %s: %w", path, err)
		}
	}
	return nil
}

func writeModule(outDir, repoRoot, revision, modPath, version, subdir string) error {
	var buf bytes.Buffer
	m := module.Version{Path: modPath, Version: version}
	if err := modzip.CreateFromVCS(&buf, m, repoRoot, revision, subdir); err != nil {
		return err
	}
	return writeProxyFiles(outDir, modPath, version, buf.Bytes())
}

func writeModuleWorktree(outDir, modPath, version, srcDir string, license []byte) error {
	pkgDir := srcDir
	if license != nil {
		if _, err := os.Stat(filepath.Join(srcDir, "LICENSE")); os.IsNotExist(err) {
			tmp, err := os.MkdirTemp("", "modpkg-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(tmp) //nolint:errcheck // cleanup of a temporary module package
			if err := os.CopyFS(tmp, os.DirFS(srcDir)); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(tmp, "LICENSE"), license, 0o600); err != nil { // #nosec G703 -- path derived from the trusted repo/module layout
				return err
			}
			pkgDir = tmp
		}
	}
	var buf bytes.Buffer
	m := module.Version{Path: modPath, Version: version}
	if err := modzip.CreateFromDir(&buf, m, pkgDir); err != nil {
		return err
	}
	return writeProxyFiles(outDir, modPath, version, buf.Bytes())
}

func writeProxyFiles(outDir, modPath, version string, zipBytes []byte) error {
	enc, err := module.EscapePath(modPath)
	if err != nil {
		return err
	}
	vdir := filepath.Join(outDir, filepath.FromSlash(enc), "@v")
	if err := os.MkdirAll(vdir, 0o750); err != nil {
		return err
	}
	gomod, err := goModFromZip(zipBytes, modPath, version)
	if err != nil {
		return err
	}
	info, err := json.Marshal(struct{ Version, Time string }{version, fixedInfoTime})
	if err != nil {
		return err
	}
	// Proxy filenames use escaped versions; metadata retains the canonical version.
	ev, err := module.EscapeVersion(version)
	if err != nil {
		return err
	}
	writes := []struct {
		name string
		data []byte
	}{
		{ev + ".zip", zipBytes},
		{ev + ".mod", gomod},
		{ev + ".info", info},
		{"list", []byte(version + "\n")},
	}
	for _, w := range writes {
		if err := os.WriteFile(filepath.Join(vdir, w.name), w.data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func goModFromZip(zipBytes []byte, modPath, version string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		return nil, err
	}
	want := path.Join(modPath+"@"+version, "go.mod")
	for _, f := range zr.File {
		if f.Name == want {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close() //nolint:errcheck // closing a read-only zip entry
			return io.ReadAll(rc)
		}
	}
	return nil, fmt.Errorf("go.mod not found in zip for %s@%s", modPath, version)
}

// Env isolates module caching and resolves unpublished fabrik modules through proxyDir.
func Env(proxyDir, modcache string) []string {
	proxyURL := "file://" + filepath.ToSlash(proxyDir)
	return []string{
		"GOWORK=off",
		"GOPROXY=" + proxyURL + ",https://proxy.golang.org",
		"GONOSUMDB=github.com/gofabrik/fabrik/*",
		"GOMODCACHE=" + modcache,
	}
}

// URL returns the file proxy URL for proxyDir.
func URL(proxyDir string) string {
	return "file://" + filepath.ToSlash(proxyDir)
}

func zipEntries(zipBytes []byte) ([]string, error) {
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	return names, nil
}
