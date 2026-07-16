// Package binaries builds reproducible, versioned Fabrik CLI archives.
package binaries

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/gofabrik/fabrik/internal/tools/candidateproxy"
	"github.com/gofabrik/fabrik/internal/tools/modset"
)

const cliModule = "github.com/gofabrik/fabrik/fabrik"

// targets must match install.sh's uname mapping.
var targets = []struct{ OS, Arch string }{
	{"darwin", "amd64"},
	{"darwin", "arm64"},
	{"linux", "amd64"},
	{"linux", "arm64"},
}

// Build writes reproducible CLI archives with versioned module metadata and checksums.
func Build(cfg *modset.Config, outDir string) error {
	if !cfg.Published[cliModule] {
		return fmt.Errorf("%s is not a published module", cliModule)
	}
	proxy, err := os.MkdirTemp("", "release-proxy-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(proxy)
	if err := candidateproxy.BuildWorktree(cfg, proxy); err != nil {
		return err
	}
	// Module downloads are target-independent.
	modcache, err := os.MkdirTemp("", "release-modcache-")
	if err != nil {
		return err
	}
	defer func() { makeWritable(modcache); os.RemoveAll(modcache) }()

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}

	var sums []string
	for _, t := range targets {
		name := fmt.Sprintf("fabrik_%s_%s.tar.gz", t.OS, t.Arch)
		sum, err := buildTarget(cfg, proxy, modcache, t.OS, t.Arch, filepath.Join(outDir, name))
		if err != nil {
			return fmt.Errorf("%s/%s: %w", t.OS, t.Arch, err)
		}
		sums = append(sums, fmt.Sprintf("%s  %s", sum, name))
	}
	sort.Strings(sums)
	return os.WriteFile(filepath.Join(outDir, "checksums.txt"), []byte(strings.Join(sums, "\n")+"\n"), 0o644)
}

func buildTarget(cfg *modset.Config, proxy, modcache, goos, goarch, dst string) (string, error) {
	gopath, err := os.MkdirTemp("", "release-gopath-")
	if err != nil {
		return "", err
	}
	defer func() { makeWritable(gopath); os.RemoveAll(gopath) }()

	env := append(os.Environ(), candidateproxy.Env(proxy, modcache)...)
	env = append(env,
		"GOPATH="+gopath,
		"GOBIN=", // Cross-compilation requires an empty GOBIN.
		"GOOS="+goos,
		"GOARCH="+goarch,
		"CGO_ENABLED=0",
	)
	cmd := exec.Command("go", "install", "-trimpath", cliModule+"@"+cfg.Version)
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go install: %w\n%s", err, out)
	}

	// Native installs use bin/; cross-installs use bin/<os>_<arch>/.
	bin := filepath.Join(gopath, "bin", goos+"_"+goarch, "fabrik")
	if goos == runtime.GOOS && goarch == runtime.GOARCH {
		bin = filepath.Join(gopath, "bin", "fabrik")
	}
	return writeArchive(dst, bin)
}

// writeArchive creates a deterministic executable archive and returns its SHA-256.
func writeArchive(dst, binPath string) (string, error) {
	data, err := os.ReadFile(binPath)
	if err != nil {
		return "", err
	}
	f, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	// Zero-valued gzip headers are deterministic.
	gz := gzip.NewWriter(io.MultiWriter(f, h))
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{
		Name:     "fabrik",
		Mode:     0o755,
		Size:     int64(len(data)),
		ModTime:  time.Unix(0, 0).UTC(),
		Typeflag: tar.TypeReg,
		Format:   tar.FormatUSTAR, // avoids PAX extended headers
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return "", err
	}
	if _, err := tw.Write(data); err != nil {
		return "", err
	}
	if err := tw.Close(); err != nil {
		return "", err
	}
	if err := gz.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// makeWritable prepares the read-only module cache for removal.
func makeWritable(root string) {
	filepath.WalkDir(root, func(p string, _ os.DirEntry, err error) error {
		if err == nil {
			os.Chmod(p, 0o755)
		}
		return nil
	})
}
