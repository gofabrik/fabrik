package assetmapper

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// vendorDownloadConcurrency bounds parallel package downloads.
const vendorDownloadConcurrency = 8

// VendorDir is the conventional subdirectory for vendored package files.
const VendorDir = "vendor"

// PackageRequest names one package version. Empty Version asks the resolver for latest.
type PackageRequest struct {
	Name    string
	Version string
}

// ResolvedPackage is one concrete package file returned by a [PackageResolver].
type ResolvedPackage struct {
	Specifier string
	Version   string
	Type      string // "js" (default) or "css"
	URL       string
}

// Resolution contains the full transitive package set needed by the browser.
type Resolution struct {
	Packages []ResolvedPackage
}

// FetchedPackage contains downloaded bytes and the final URL after redirects.
type FetchedPackage struct {
	Content   []byte
	SourceURL string
}

// PackageResolver resolves package requests and downloads package files.
type PackageResolver interface {
	Resolve(ctx context.Context, reqs []PackageRequest) (*Resolution, error)
	Fetch(ctx context.Context, url string) ([]byte, error)
}

// ProvenancePackageResolver reports the final source URL for a download.
type ProvenancePackageResolver interface {
	FetchPackage(ctx context.Context, url string) (*FetchedPackage, error)
}

// Vendor manages package files referenced by an [Importmap].
//
// Vendor methods are not safe for concurrent use.
//
// After mutating, callers persist the importmap with [Importmap.Save].
type Vendor struct {
	// Resolver supplies the upstream resolution + download. Required.
	Resolver PackageResolver
	// VendorDir is the on-disk directory where vendored files live.
	// For a project whose asset root is <project>/assets, this is
	// typically <project>/assets/vendor.
	VendorDir string
	// Importmap holds the in-memory importmap. Mutated by Require /
	// Remove. Required.
	Importmap *Importmap
	// Lockfile is the vendoring provenance file. Empty uses
	// VendorLockFilename beside VendorDir.
	Lockfile string
	// Limits bounds package count and downloaded bytes.
	Limits DownloadLimits
}

// Require vendors pkg@version and registers its transitive dependencies.
func (v *Vendor) Require(ctx context.Context, pkg, version string) error {
	if err := v.validate(); err != nil {
		return err
	}
	if pkg == "" {
		return fmt.Errorf("assetmapper.Vendor.Require: empty package name")
	}
	res, err := v.Resolver.Resolve(ctx, []PackageRequest{{Name: pkg, Version: version}})
	if err != nil {
		return fmt.Errorf("assetmapper.Vendor.Require: resolve %s: %w", pkg, err)
	}
	return v.applyResolution(ctx, res)
}

// Remove deletes one vendored package entry. It does not remove transitive dependencies.
func (v *Vendor) Remove(specifier string) error {
	if err := v.validate(); err != nil {
		return err
	}
	dst, err := v.ValidateRemove(specifier)
	if err != nil {
		return err
	}
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("assetmapper.Vendor.Remove: %s: %w", dst, err)
	}
	lock, exists, err := v.loadLock()
	if err != nil {
		return err
	}
	if exists {
		delete(lock.Packages, specifier)
		if err := lock.Save(v.lockPath()); err != nil {
			return err
		}
	}
	delete(v.Importmap.Entries, specifier)
	return nil
}

// ValidateRemove returns the file [Vendor.Remove] would delete without mutating state.
func (v *Vendor) ValidateRemove(specifier string) (string, error) {
	if err := v.validate(); err != nil {
		return "", err
	}
	entry, ok := v.Importmap.Entries[specifier]
	if !ok {
		return "", fmt.Errorf("assetmapper.Vendor.Remove: %q not in importmap", specifier)
	}
	if entry.Version == "" {
		return "", fmt.Errorf("assetmapper.Vendor.Remove: %q is a local entry (no version) — edit importmap.json directly", specifier)
	}
	rel, err := vendorRelPath(specifier, entry.Type)
	if err != nil {
		return "", fmt.Errorf("assetmapper.Vendor.Remove: %w", err)
	}
	return filepath.Join(v.VendorDir, filepath.FromSlash(rel)), nil
}

// vendorRelPath maps a bare specifier under VendorDir and rejects traversal.
func vendorRelPath(specifier, typ string) (string, error) {
	ext := ".js"
	if typ == "css" {
		ext = ".css"
	}
	// fs.ValidPath admits ".", but a specifier must name a file.
	if specifier == "." || !fs.ValidPath(specifier) || strings.ContainsRune(specifier, '\\') {
		return "", fmt.Errorf("specifier %q does not map to a safe path under the vendor directory", specifier)
	}
	return specifier + ext, nil
}

// Prune deletes vendored files that no importmap entry references.
//
// Returned paths are relative to VendorDir and sorted. Prune never edits importmap.json.
func (v *Vendor) Prune() ([]string, error) {
	if err := v.validate(); err != nil {
		return nil, err
	}

	expected := make(map[string]struct{}, len(v.Importmap.Entries))
	expectedSpecs := make(map[string]struct{}, len(v.Importmap.Entries))
	for spec, entry := range v.Importmap.Entries {
		if entry.Version == "" {
			continue // local entries don't live under VendorDir
		}
		ext := ".js"
		if entry.Type == "css" {
			ext = ".css"
		}
		expected[filepath.FromSlash(spec+ext)] = struct{}{}
		expectedSpecs[spec] = struct{}{}
	}

	if _, err := os.Stat(v.VendorDir); err != nil {
		if os.IsNotExist(err) {
			return nil, v.pruneLock(expectedSpecs)
		}
		return nil, fmt.Errorf("assetmapper.Vendor.Prune: stat %s: %w", v.VendorDir, err)
	}

	var removed []string
	err := filepath.WalkDir(v.VendorDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(v.VendorDir, p)
		if err != nil {
			return err
		}
		if _, keep := expected[rel]; keep {
			return nil
		}
		if err := os.Remove(p); err != nil {
			return fmt.Errorf("remove %s: %w", p, err)
		}
		removed = append(removed, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return removed, fmt.Errorf("assetmapper.Vendor.Prune: %w", err)
	}

	// Directory cleanup is best-effort; Prune's contract is about files.
	pruneEmptyDirs(v.VendorDir)
	if err := v.pruneLock(expectedSpecs); err != nil {
		return removed, err
	}

	sort.Strings(removed)
	return removed, nil
}

func (v *Vendor) pruneLock(expected map[string]struct{}) error {
	lock, exists, err := v.loadLock()
	if err != nil || !exists {
		return err
	}
	changed := false
	for specifier := range lock.Packages {
		if _, keep := expected[specifier]; keep {
			continue
		}
		delete(lock.Packages, specifier)
		changed = true
	}
	if !changed {
		return nil
	}
	return lock.Save(v.lockPath())
}

// pruneEmptyDirs removes empty subdirectories of root, bottom-up.
func pruneEmptyDirs(root string) {
	var dirs []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && p != root {
			dirs = append(dirs, p)
		}
		return nil
	})
	// Deepest first so parent directories can become empty in the same pass.
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, d := range dirs {
		_ = os.Remove(d)
	}
}

func (v *Vendor) validate() error {
	if v.Resolver == nil {
		return fmt.Errorf("assetmapper.Vendor: nil Resolver")
	}
	if v.Importmap == nil {
		return fmt.Errorf("assetmapper.Vendor: nil Importmap")
	}
	if v.VendorDir == "" {
		return fmt.Errorf("assetmapper.Vendor: empty VendorDir")
	}
	return nil
}

// applyResolution fetches every package before writing files or importmap entries.
//
// Disk failures can leave partial files; importmap entries are committed only after every write succeeds.
func (v *Vendor) applyResolution(ctx context.Context, res *Resolution) error {
	if res == nil {
		return fmt.Errorf("assetmapper.Vendor: resolver returned nil resolution")
	}
	if len(res.Packages) == 0 {
		return nil
	}
	limits := v.Limits.normalized()
	if len(res.Packages) > limits.MaxPackages {
		return fmt.Errorf("assetmapper.Vendor: resolved %d packages, limit is %d", len(res.Packages), limits.MaxPackages)
	}

	// Validate every destination path before network or disk work.
	rels := make([]string, len(res.Packages))
	packages := make([]ResolvedPackage, len(res.Packages))
	seen := make(map[string]struct{}, len(res.Packages))
	for i, p := range res.Packages {
		if _, duplicate := seen[p.Specifier]; duplicate {
			return fmt.Errorf("assetmapper.Vendor: duplicate resolved package %q", p.Specifier)
		}
		seen[p.Specifier] = struct{}{}
		if p.Version == "" {
			return fmt.Errorf("assetmapper.Vendor: package %q has no exact version", p.Specifier)
		}
		if p.Type == "" {
			p.Type = "js"
		}
		if p.Type != "js" && p.Type != "css" {
			return fmt.Errorf("assetmapper.Vendor: package %q has invalid type %q", p.Specifier, p.Type)
		}
		if p.URL == "" {
			return fmt.Errorf("assetmapper.Vendor: package %q has no source URL", p.Specifier)
		}
		rel, err := vendorRelPath(p.Specifier, p.Type)
		if err != nil {
			return fmt.Errorf("assetmapper.Vendor: %w", err)
		}
		rels[i] = rel
		packages[i] = p
	}

	urlToSpec := make(map[string]string, len(packages))
	for _, p := range packages {
		urlToSpec[p.URL] = p.Specifier
	}

	lock, exists, err := v.loadLock()
	if err != nil {
		return err
	}
	if exists {
		if err := lock.Verify(v.VendorDir); err != nil {
			return err
		}
	} else {
		lock = newVendorLock()
	}
	for specifier, entry := range v.Importmap.Entries {
		if entry.Version == "" {
			continue
		}
		locked, hasLock := lock.Packages[specifier]
		if hasLock {
			entryType := entry.Type
			if entryType == "" {
				entryType = "js"
			}
			if _, resolved := seen[specifier]; !resolved &&
				(locked.Version != entry.Version || locked.Type != entryType) {
				return fmt.Errorf(
					"assetmapper.Vendor: provenance for existing package %q does not match importmap version and type",
					specifier,
				)
			}
			continue
		}
		if _, resolved := seen[specifier]; !resolved {
			return fmt.Errorf(
				"assetmapper.Vendor: existing vendored package %q has no provenance; re-resolve the complete vendored set before adding packages",
				specifier,
			)
		}
	}

	// Fetch everything in memory before mutating disk or importmap state.
	staged, err := v.fetchAll(ctx, packages, rels, urlToSpec, limits)
	if err != nil {
		return err
	}
	for _, item := range staged {
		if err := validateLockedPackage(item.pkg.Specifier, item.locked); err != nil {
			return fmt.Errorf("assetmapper.Vendor: %w", err)
		}
		lock.Packages[item.pkg.Specifier] = item.locked
	}

	// Write files before importmap entries so missing-file entries cannot persist.
	if err := os.MkdirAll(v.VendorDir, 0o755); err != nil { // #nosec G301 -- served asset, world-readable by design
		return fmt.Errorf("assetmapper.Vendor: create %s: %w", v.VendorDir, err)
	}
	for _, s := range staged {
		if err := os.MkdirAll(filepath.Dir(s.dst), 0o755); err != nil { // #nosec G301 -- served asset, world-readable by design
			return fmt.Errorf("assetmapper.Vendor: mkdir for %s: %w", s.pkg.Specifier, err)
		}
		if err := os.WriteFile(s.dst, s.content, 0o644); err != nil { // #nosec G306 -- served asset, world-readable by design
			return fmt.Errorf("assetmapper.Vendor: write %s: %w", s.dst, err)
		}
	}
	if err := lock.Save(v.lockPath()); err != nil {
		return err
	}

	// In-process map mutation cannot fail.
	for _, s := range staged {
		v.Importmap.Entries[s.pkg.Specifier] = ImportmapEntry{
			Version: s.pkg.Version,
			Type:    s.pkg.Type,
		}
	}
	return nil
}

// stagedPackage is one fetched package ready to write.
type stagedPackage struct {
	pkg     ResolvedPackage
	content []byte
	dst     string
	locked  LockedPackage
}

// fetchAll downloads packages in parallel and cancels remaining work on the first error.
func (v *Vendor) fetchAll(
	ctx context.Context,
	pkgs []ResolvedPackage,
	rels []string,
	urlToSpec map[string]string,
	limits DownloadLimits,
) ([]stagedPackage, error) {
	staged := make([]stagedPackage, len(pkgs))

	derived, cancel := context.WithCancel(ctx)
	defer cancel()

	sem := make(chan struct{}, vendorDownloadConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	var totalSourceBytes int64
	var totalPublishedBytes int64

dispatch:
	for i, p := range pkgs {
		select {
		case <-derived.Done():
			break dispatch
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(i int, p ResolvedPackage) {
			defer wg.Done()
			defer func() { <-sem }()

			fetched := &FetchedPackage{SourceURL: p.URL}
			var err error
			if resolver, ok := v.Resolver.(ProvenancePackageResolver); ok {
				fetched, err = resolver.FetchPackage(derived, p.URL)
			} else {
				fetched.Content, err = v.Resolver.Fetch(derived, p.URL)
			}
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("assetmapper.Vendor: fetch %s: %w", p.URL, err)
					cancel()
				}
				mu.Unlock()
				return
			}
			if fetched == nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("assetmapper.Vendor: fetch %s: resolver returned nil result", p.URL)
					cancel()
				}
				mu.Unlock()
				return
			}
			if fetched.SourceURL == "" {
				fetched.SourceURL = p.URL
			}
			size := int64(len(fetched.Content))
			mu.Lock()
			if size > limits.MaxPackageBytes && firstErr == nil {
				firstErr = fmt.Errorf("assetmapper.Vendor: fetch %s: package exceeds %d-byte limit", p.URL, limits.MaxPackageBytes)
				cancel()
			} else if size > limits.MaxResolutionBytes-totalSourceBytes && firstErr == nil {
				firstErr = fmt.Errorf("assetmapper.Vendor: downloaded resolution exceeds %d-byte limit", limits.MaxResolutionBytes)
				cancel()
			} else if firstErr == nil {
				totalSourceBytes += size
			}
			fetchErr := firstErr
			mu.Unlock()
			if fetchErr != nil {
				return
			}
			content := fetched.Content
			sourceSum := sha256.Sum256(content)
			publishedSize := size
			var refs []ref
			if strings.HasSuffix(rels[i], ".js") {
				refs, publishedSize, err = planVendoredJS(content, urlToSpec, limits.MaxPackageBytes)
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("assetmapper.Vendor: rewrite %s: %w", p.Specifier, err)
						cancel()
					}
					mu.Unlock()
					return
				}
			}
			mu.Lock()
			if publishedSize > limits.MaxPackageBytes && firstErr == nil {
				firstErr = fmt.Errorf("assetmapper.Vendor: published package %s exceeds %d-byte limit", p.Specifier, limits.MaxPackageBytes)
				cancel()
			} else if publishedSize > limits.MaxResolutionBytes-totalPublishedBytes && firstErr == nil {
				firstErr = fmt.Errorf("assetmapper.Vendor: published resolution exceeds %d-byte limit", limits.MaxResolutionBytes)
				cancel()
			} else if firstErr == nil {
				totalPublishedBytes += publishedSize
			}
			publishErr := firstErr
			mu.Unlock()
			if publishErr != nil {
				return
			}
			if refs != nil {
				content = rewriteRefs(content, refs, func(r ref) string {
					return urlToSpec[r.spec]
				})
			}
			sum := sha256.Sum256(content)
			staged[i] = stagedPackage{
				pkg:     p,
				content: content,
				dst:     filepath.Join(v.VendorDir, filepath.FromSlash(rels[i])),
				locked: LockedPackage{
					Version:      p.Version,
					Type:         p.Type,
					SourceURL:    fetched.SourceURL,
					SourceSize:   size,
					SourceSHA256: hex.EncodeToString(sourceSum[:]),
					Size:         publishedSize,
					SHA256:       hex.EncodeToString(sum[:]),
				},
			}
		}(i, p)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return staged, nil
}

func (v *Vendor) lockPath() string {
	if v.Lockfile != "" {
		return v.Lockfile
	}
	return filepath.Join(filepath.Dir(filepath.Clean(v.VendorDir)), VendorLockFilename)
}

func (v *Vendor) loadLock() (*VendorLock, bool, error) {
	lock, err := LoadVendorLock(v.lockPath())
	if err == nil {
		return lock, true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	return nil, false, err
}

// rewriteVendoredJS replaces known upstream URLs with their bare specifiers.
func rewriteVendoredJS(content []byte, urlToSpec map[string]string) []byte {
	refs, _, err := planVendoredJS(content, urlToSpec, int64(^uint64(0)>>1))
	if err != nil {
		return content
	}
	return rewriteRefs(content, refs, func(r ref) string {
		return urlToSpec[r.spec]
	})
}

// planVendoredJS finds URL rewrites and computes their final size before
// rewriteRefs allocates the published artifact.
func planVendoredJS(content []byte, urlToSpec map[string]string, limit int64) ([]ref, int64, error) {
	var refs []ref
	size := int64(len(content))
	for _, m := range jsImportRE.FindAllSubmatchIndex(content, -1) {
		spec := string(content[m[2]:m[3]])
		replacement, ok := urlToSpec[spec]
		if !ok {
			continue
		}
		delta := int64(len(replacement)) - int64(m[3]-m[2])
		if delta > 0 && size > limit-delta {
			return nil, 0, fmt.Errorf("published package exceeds %d-byte limit", limit)
		}
		size += delta
		refs = append(refs, ref{
			spec:     spec,
			resolved: spec,
			start:    m[2],
			end:      m[3],
		})
	}
	if size > limit {
		return nil, 0, fmt.Errorf("published package exceeds %d-byte limit", limit)
	}
	return refs, size, nil
}
