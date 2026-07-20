package assetmapper

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// Compile writes a content-hashed asset tree and manifest to publicDir.
//
// Resolution semantics match [Mapper.Asset]: roots are walked in
// order; a logical path discovered in an earlier root shadows the
// same path in later roots.
//
// publicDir is created if missing. Existing compiled files are overwritten, not pruned.
//
// Reference rewriting only touches paths that resolve to a known asset.
//
// Top-level importmap.json is configuration, not an asset.
//
// The URL prefix is baked into rewritten references and persisted in [Manifest.URLPrefix].
//
// Compile is not safe for concurrent invocation against the same publicDir.
func Compile(srcRoots []Root, publicDir string, opts ...BuildOption) (*Manifest, error) {
	srcRoots, err := normalizeRoots("assetmapper.Compile", srcRoots)
	if err != nil {
		return nil, err
	}
	var cfg buildConfig
	for _, o := range opts {
		o(&cfg)
	}
	prefix, err := normalizeURLPrefix(cfg.urlPrefix)
	if err != nil {
		return nil, fmt.Errorf("assetmapper.Compile: %w", err)
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

	// JS and CSS are read for rewriting; other assets stay on disk.
	assets, err := collectAssets(srcRoots)
	if err != nil {
		return nil, fmt.Errorf("assetmapper.Compile: %w", err)
	}

	hashedNames := make(map[string]string, len(assets))
	outputOwner := make(map[string]string, len(assets))

	// Non-JS/CSS assets hash independently of the dependency graph.
	var streamables []string
	for logical, a := range assets {
		if a.kind != kindJS && a.kind != kindCSS {
			streamables = append(streamables, logical)
		}
	}
	sort.Strings(streamables)
	for _, logical := range streamables {
		a := assets[logical]
		hash, tmpPath, err := streamHashWrite(a.root.FS, a.subPath, publicDir)
		if err != nil {
			return nil, fmt.Errorf("assetmapper.Compile: stream %s: %w", logical, err)
		}
		hashed := hashedName(logical, hash)
		if cerr := checkCollision("assetmapper.Compile", logical, hashed, assets, outputOwner); cerr != nil {
			_ = os.Remove(tmpPath)
			return nil, cerr
		}
		dst := filepath.Join(publicDir, filepath.FromSlash(hashed))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil { // #nosec G301 -- served asset, world-readable by design
			_ = os.Remove(tmpPath)
			return nil, fmt.Errorf("assetmapper.Compile: mkdir for %s: %w", logical, err)
		}
		if err := os.Rename(tmpPath, dst); err != nil {
			_ = os.Remove(tmpPath)
			return nil, fmt.Errorf("assetmapper.Compile: rename %s: %w", dst, err)
		}
		outputOwner[hashed] = logical
		hashedNames[logical] = hashed
	}

	// Only JS/CSS assets participate in the dependency graph.
	deps := make(map[string][]string)
	refsByAsset := make(map[string][]ref)
	for logical, a := range assets {
		if a.kind != kindJS && a.kind != kindCSS {
			continue
		}
		deps[logical] = nil
		refs := extractRefs(logical, a.content, a.kind)
		refsByAsset[logical] = refs
		for _, r := range refs {
			if r.resolved == "" {
				continue
			}
			target, ok := assets[r.resolved]
			if !ok {
				continue
			}
			if target.kind != kindJS && target.kind != kindCSS {
				continue
			}
			deps[logical] = append(deps[logical], r.resolved)
		}
	}

	order, err := topoSort(deps)
	if err != nil {
		return nil, err
	}

	// JS and CSS hash after rewriting against final dependency URLs.
	for _, logical := range order {
		a := assets[logical]
		rewritten := a.content
		if refs := refsByAsset[logical]; len(refs) > 0 {
			rewritten = rewriteRefs(a.content, refs, func(r ref) string {
				target, ok := hashedNames[r.resolved]
				if !ok {
					return r.spec
				}
				return prefix + target + r.suffix
			})
		}
		hash := hashContent(rewritten)
		hashed := hashedName(logical, hash)
		if cerr := checkCollision("assetmapper.Compile", logical, hashed, assets, outputOwner); cerr != nil {
			return nil, cerr
		}
		outputOwner[hashed] = logical
		hashedNames[logical] = hashed

		dst := filepath.Join(publicDir, filepath.FromSlash(hashed))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil { // #nosec G301 -- served asset, world-readable by design
			return nil, fmt.Errorf("assetmapper.Compile: mkdir for %s: %w", logical, err)
		}
		if err := os.WriteFile(dst, rewritten, 0o644); err != nil { // #nosec G306 -- served asset, world-readable by design
			return nil, fmt.Errorf("assetmapper.Compile: write %s: %w", dst, err)
		}
	}

	manifest := NewManifest()
	manifest.URLPrefix = prefix
	for logical, hashed := range hashedNames {
		manifest.Entries[logical] = hashed
	}
	if err := manifest.Save(publicDir); err != nil {
		return nil, err
	}
	return manifest, nil
}

// checkCollision rejects output names that cannot be served unambiguously.
//
//  1. Two distinct logical paths producing the same compiled
//     filename (8-char SHA-256 collision or adversarial naming).
//  2. The compiled filename equals the literal source path of
//     another asset (e.g. foo.js hashes to 12345678 and a source
//     file foo-12345678.js also exists). Indistinguishable at the
//     URL level and confusing in publicDir.
func checkCollision(context, logical, hashed string, assets map[string]*collectedAsset, outputOwner map[string]string) error {
	if other, dup := outputOwner[hashed]; dup {
		return fmt.Errorf("%s: %q and %q both compile to %q (hash collision or naming overlap); rename one of them",
			context, other, logical, hashed)
	}
	if _, shadowed := assets[hashed]; shadowed {
		return fmt.Errorf("%s: %q compiles to %q, which is also a literal source path; rename one of them",
			context, logical, hashed)
	}
	return nil
}

// collectedAsset is an asset prepared for compile-time hashing.
type collectedAsset struct {
	root    Root
	subPath string
	kind    assetKind
	content []byte // nil for non-JS/CSS (streamed instead)
}

func collectAssets(roots []Root) (map[string]*collectedAsset, error) {
	assets := make(map[string]*collectedAsset)
	for _, root := range roots {
		err := fs.WalkDir(root.FS, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			// Top-level importmap.json is configuration, not an asset.
			if p == ImportmapFilename {
				return nil
			}
			logical := p
			if root.MountAt != "" {
				logical = root.MountAt + "/" + p
			}
			if _, dup := assets[logical]; dup {
				return nil
			}
			kind := kindOf(logical)
			ca := &collectedAsset{root: root, subPath: p, kind: kind}
			if kind == kindJS || kind == kindCSS {
				content, err := fs.ReadFile(root.FS, p)
				if err != nil {
					return fmt.Errorf("read %s: %w", p, err)
				}
				ca.content = content
			}
			assets[logical] = ca
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walk: %w", err)
		}
	}
	return assets, nil
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
