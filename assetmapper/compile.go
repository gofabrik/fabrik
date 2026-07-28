package assetmapper

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Compile writes a content-hashed asset tree and manifest to publicDir.
//
// Resolution semantics match [Mapper.Asset]: roots are walked in
// order; a logical path discovered in an earlier root shadows the
// same path in later roots.
//
// publicDir is created if missing. Existing compiled files are overwritten, not pruned.
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

	// Remove only temp files with the exact assetmapper prefix.
	if stale, _ := filepath.Glob(filepath.Join(publicDir, ".assetmapper-tmp-*.tmp")); len(stale) > 0 {
		for _, p := range stale {
			_ = os.Remove(p)
		}
	}

	for _, logical := range plan.order {
		asset := plan.assets[logical]
		dst := filepath.Join(publicDir, filepath.FromSlash(asset.output))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil { // #nosec G301 -- served asset, world-readable by design
			return nil, fmt.Errorf("assetmapper.Compile: mkdir for %s: %w", logical, err)
		}
		if asset.kind == kindJS || asset.kind == kindCSS {
			if err := os.WriteFile(dst, asset.content, 0o644); err != nil { // #nosec G306 -- served asset, world-readable by design
				return nil, fmt.Errorf("assetmapper.Compile: write %s: %w", dst, err)
			}
			continue
		}

		hash, tmpPath, err := streamHashWrite(asset.root.FS, asset.subPath, publicDir)
		if err != nil {
			return nil, fmt.Errorf("assetmapper.Compile: stream %s: %w", logical, err)
		}
		if hash != asset.hash {
			_ = os.Remove(tmpPath)
			return nil, fmt.Errorf("assetmapper.Compile: source %s changed while compiling", logical)
		}
		if err := os.Rename(tmpPath, dst); err != nil {
			_ = os.Remove(tmpPath)
			return nil, fmt.Errorf("assetmapper.Compile: rename %s: %w", dst, err)
		}
	}

	if err := plan.manifest.Save(publicDir); err != nil {
		return nil, err
	}
	return plan.manifest, nil
}

// streamHashWrite writes a source file to a temp file while hashing it.
//
// The caller renames or removes the returned temp file.
func streamHashWrite(srcFS fs.FS, srcPath, publicDir string) (hash, tmpPath string, err error) {
	tmp, err := os.CreateTemp(publicDir, ".assetmapper-tmp-*.tmp")
	if err != nil {
		return "", "", err
	}
	tmpName := tmp.Name()

	src, err := srcFS.Open(srcPath)
	if err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", "", err
	}
	defer src.Close() //nolint:errcheck // read-only source close cannot affect the completed copy

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), src); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", "", err
	}
	return hex.EncodeToString(h.Sum(nil))[:HashLength], tmpName, nil
}
