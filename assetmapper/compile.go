package assetmapper

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Compile writes a hash-addressed asset tree and manifest to publicDir.
//
// Resolution semantics match [Mapper.Asset]: roots are walked in
// order; a logical path discovered in an earlier root shadows the
// same path in later roots.
//
// publicDir is created if missing. Existing compiled files are retained. A
// complete release is staged first, content-addressed files are published
// without overwriting existing bytes, and manifest.json is atomically replaced
// last. Therefore a failed compile leaves the previous manifest usable.
//
// Compile discovers and validates a top-level importmap.json using the same
// planning pipeline as [Build] and [Check].
//
// Top-level importmap.json is configuration, not an asset.
//
// The URL prefix is baked into rewritten references and persisted in [Manifest.URLPrefix].
//
// Compile is not safe for concurrent invocation against the same publicDir.
func Compile(srcRoots []Root, publicDir string, opts ...BuildOption) (*Manifest, error) {
	plan, err := planBuild("assetmapper.Compile", srcRoots, nil, opts)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(publicDir, 0o755); err != nil { // #nosec G301 -- served asset, world-readable by design
		return nil, fmt.Errorf("assetmapper.Compile: create publicDir: %w", err)
	}
	cleanupCompileTemps(publicDir)

	stageDir, err := os.MkdirTemp(publicDir, ".assetmapper-build-*.staging")
	if err != nil {
		return nil, fmt.Errorf("assetmapper.Compile: create staging directory: %w", err)
	}
	defer os.RemoveAll(stageDir) //nolint:errcheck // best-effort cleanup after publish or failure

	for _, logical := range plan.order {
		asset := plan.assets[logical]
		dst := filepath.Join(stageDir, filepath.FromSlash(asset.output))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil { // #nosec G301 -- served asset, world-readable by design
			return nil, fmt.Errorf("assetmapper.Compile: stage mkdir for %s: %w", logical, err)
		}
		if asset.kind == kindJS || asset.kind == kindCSS {
			if err := writeSyncedFile(dst, asset.content, 0o644); err != nil {
				return nil, fmt.Errorf("assetmapper.Compile: stage %s: %w", logical, err)
			}
			continue
		}

		hash, err := streamHashWrite(asset.root.FS, asset.subPath, dst)
		if err != nil {
			return nil, fmt.Errorf("assetmapper.Compile: stream %s: %w", logical, err)
		}
		if hash != asset.outputHash {
			return nil, fmt.Errorf("assetmapper.Compile: source %s changed while compiling", logical)
		}
	}

	if err := plan.manifest.Save(stageDir); err != nil {
		return nil, err
	}
	if err := publishCompileStage(stageDir, publicDir, plan); err != nil {
		return nil, err
	}
	return plan.manifest, nil
}

func cleanupCompileTemps(publicDir string) {
	entries, err := os.ReadDir(publicDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() && strings.HasPrefix(name, ".assetmapper-build-") &&
			strings.HasSuffix(name, ".staging") {
			_ = os.RemoveAll(filepath.Join(publicDir, name))
			continue
		}
		if !entry.IsDir() && strings.HasPrefix(name, ".assetmapper-tmp-") &&
			strings.HasSuffix(name, ".tmp") {
			_ = os.Remove(filepath.Join(publicDir, name))
		}
	}
}

func publishCompileStage(stageDir, publicDir string, plan *buildPlan) error {
	for _, logical := range plan.order {
		asset := plan.assets[logical]
		staged := filepath.Join(stageDir, filepath.FromSlash(asset.output))
		dst := filepath.Join(publicDir, filepath.FromSlash(asset.output))
		if _, err := os.Stat(dst); err == nil {
			equal, err := equalFiles(staged, dst)
			if err != nil {
				return fmt.Errorf("assetmapper.Compile: compare existing %s: %w", dst, err)
			}
			if !equal {
				return fmt.Errorf("assetmapper.Compile: existing content-addressed file %s has different bytes", dst)
			}
			continue
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("assetmapper.Compile: stat %s: %w", dst, err)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil { // #nosec G301 -- served asset, world-readable by design
			return fmt.Errorf("assetmapper.Compile: mkdir for %s: %w", logical, err)
		}
		if err := os.Rename(staged, dst); err != nil {
			return fmt.Errorf("assetmapper.Compile: publish %s: %w", logical, err)
		}
		if err := syncAncestorDirectories(filepath.Dir(dst), publicDir); err != nil {
			return fmt.Errorf("assetmapper.Compile: sync published %s: %w", logical, err)
		}
	}

	stagedManifest := filepath.Join(stageDir, ManifestFilename)
	publicManifest := filepath.Join(publicDir, ManifestFilename)
	if err := os.Rename(stagedManifest, publicManifest); err != nil {
		return fmt.Errorf("assetmapper.Compile: publish manifest: %w", err)
	}
	if err := syncDirectory(publicDir); err != nil {
		return &IndeterminateCommitError{Path: publicManifest, Err: err}
	}
	return nil
}

func equalFiles(leftPath, rightPath string) (bool, error) {
	left, err := os.Open(leftPath) // #nosec G304 -- paths are validated compiler outputs
	if err != nil {
		return false, err
	}
	defer left.Close()               //nolint:errcheck // read-only comparison
	right, err := os.Open(rightPath) // #nosec G304 -- paths are validated compiler outputs
	if err != nil {
		return false, err
	}
	defer right.Close() //nolint:errcheck // read-only comparison
	leftInfo, err := left.Stat()
	if err != nil {
		return false, err
	}
	rightInfo, err := right.Stat()
	if err != nil {
		return false, err
	}
	if leftInfo.Size() != rightInfo.Size() {
		return false, nil
	}
	leftHash := sha256.New()
	if _, err := io.Copy(leftHash, left); err != nil {
		return false, err
	}
	rightHash := sha256.New()
	if _, err := io.Copy(rightHash, right); err != nil {
		return false, err
	}
	return bytes.Equal(leftHash.Sum(nil), rightHash.Sum(nil)), nil
}

func writeSyncedFile(path string, content []byte, perm os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm) // #nosec G304 -- caller-selected staging path
	if err != nil {
		return err
	}
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// streamHashWrite writes a source file to dst while hashing it.
func streamHashWrite(srcFS fs.FS, srcPath, dst string) (hash string, err error) {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) // #nosec G304 -- validated staging path
	if err != nil {
		return "", err
	}

	src, err := srcFS.Open(srcPath)
	if err != nil {
		_ = out.Close()
		return "", err
	}
	defer src.Close() //nolint:errcheck // read-only source close cannot affect the completed copy

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, h), src); err != nil {
		_ = out.Close()
		return "", err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil))[:HashLength], nil
}
