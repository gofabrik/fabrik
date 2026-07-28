package assetmapper

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
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
	// Dependencies lists bare specifiers required by this artifact. Vendor
	// also discovers dependencies from rewritten upstream URLs.
	Dependencies []string
}

// Resolution contains the full transitive package set needed by the browser.
// PackageResolver implementations should populate dependency edges when they
// cannot be discovered from rewritten source URLs.
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
// Mutations publish the provenance lock and persisted importmap as one
// recoverable transaction.
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
	// ImportmapFile is atomically updated with the vendor lock. Empty uses
	// ImportmapFilename beside VendorDir.
	ImportmapFile string
	// Limits bounds package count and downloaded bytes.
	Limits DownloadLimits
}

// Require vendors pkg@version and registers its transitive dependencies.
func (v *Vendor) Require(ctx context.Context, pkg, version string) error {
	return v.RequirePackages(ctx, []PackageRequest{{Name: pkg, Version: version}})
}

// RequirePackages resolves and publishes a package batch as one metadata
// transaction over immutable artifacts. All request names must be non-empty.
func (v *Vendor) RequirePackages(ctx context.Context, requests []PackageRequest) error {
	if err := v.validate(); err != nil {
		return err
	}
	if err := v.recoverTransaction(); err != nil {
		return err
	}
	if len(requests) == 0 {
		return nil
	}
	direct, err := v.directRequirements()
	if err != nil {
		return err
	}
	for _, request := range requests {
		if request.Name == "" {
			return fmt.Errorf("assetmapper.Vendor.RequirePackages: empty package name")
		}
		if _, err := vendorRelPath(request.Name, "js"); err != nil {
			return fmt.Errorf("assetmapper.Vendor.RequirePackages: %w", err)
		}
		direct[request.Name] = request.Version
	}
	return v.resolveAndReplace(ctx, direct)
}

// Remove unregisters one vendored package. Its immutable artifact remains until
// Prune, so metadata publication never creates a missing-file window.
func (v *Vendor) Remove(specifier string) error {
	return v.RemoveContext(context.Background(), specifier)
}

// RemoveContext unregisters one direct requirement with cancellation.
func (v *Vendor) RemoveContext(ctx context.Context, specifier string) error {
	return v.RemovePackagesContext(ctx, []string{specifier})
}

// RemovePackages unregisters a package batch as one metadata transaction.
func (v *Vendor) RemovePackages(specifiers []string) error {
	return v.RemovePackagesContext(context.Background(), specifiers)
}

// RemovePackagesContext unregisters a package batch as one cancellable
// metadata transaction.
func (v *Vendor) RemovePackagesContext(ctx context.Context, specifiers []string) error {
	if err := v.validate(); err != nil {
		return err
	}
	if err := v.recoverTransaction(); err != nil {
		return err
	}
	if len(specifiers) == 0 {
		return nil
	}
	for _, specifier := range specifiers {
		if _, err := v.ValidateRemove(specifier); err != nil {
			return err
		}
	}
	direct, err := v.directRequirements()
	if err != nil {
		return err
	}
	for _, specifier := range specifiers {
		if _, ok := direct[specifier]; !ok {
			return fmt.Errorf("assetmapper.Vendor.Remove: %q is transitive, not a direct requirement", specifier)
		}
		delete(direct, specifier)
	}
	return v.resolveAndReplace(ctx, direct)
}

// ValidateRemove validates that a specifier is a removable vendored entry and
// returns its current immutable artifact path. Remove retains that artifact
// until [Vendor.Prune].
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
	if lock, exists, err := v.loadLock(); err != nil {
		return "", err
	} else if exists {
		if _, direct := lock.DirectRequirements[specifier]; !direct {
			return "", fmt.Errorf("assetmapper.Vendor.Remove: %q is transitive, not a direct requirement", specifier)
		}
	}
	if entry.Path != "" {
		prefix := VendorDir + "/"
		if !strings.HasPrefix(entry.Path, prefix) {
			return "", fmt.Errorf("assetmapper.Vendor.Remove: %q has vendored path %q outside %s", specifier, entry.Path, prefix)
		}
		rel := strings.TrimPrefix(entry.Path, prefix)
		if rel == "." || !fs.ValidPath(rel) {
			return "", fmt.Errorf("assetmapper.Vendor.Remove: %q has invalid vendored path %q", specifier, entry.Path)
		}
		return filepath.Join(v.VendorDir, filepath.FromSlash(rel)), nil
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
	if err := v.recoverTransaction(); err != nil {
		return nil, err
	}

	expected := make(map[string]struct{}, len(v.Importmap.Entries))
	expectedSpecs := make(map[string]struct{}, len(v.Importmap.Entries))
	for spec, entry := range v.Importmap.Entries {
		if entry.Version == "" {
			continue // local entries don't live under VendorDir
		}
		if entry.Path != "" {
			prefix := VendorDir + "/"
			if !strings.HasPrefix(entry.Path, prefix) {
				return nil, fmt.Errorf("assetmapper.Vendor.Prune: %q has vendored path outside %s", spec, prefix)
			}
			rel := strings.TrimPrefix(entry.Path, prefix)
			if rel == "." || !fs.ValidPath(rel) || strings.ContainsRune(rel, '\\') {
				return nil, fmt.Errorf("assetmapper.Vendor.Prune: %q has invalid vendored path %q", spec, entry.Path)
			}
			expected[filepath.FromSlash(rel)] = struct{}{}
		} else {
			rel, err := vendorRelPath(spec, entry.Type)
			if err != nil {
				return nil, fmt.Errorf("assetmapper.Vendor.Prune: %w", err)
			}
			expected[filepath.FromSlash(rel)] = struct{}{}
		}
		expectedSpecs[spec] = struct{}{}
	}
	// Stop locking orphaned files before deleting them. An interruption can
	// then leave only harmless unreferenced bytes, never a lock that names a
	// missing artifact.
	if err := v.pruneLock(expectedSpecs); err != nil {
		return nil, err
	}

	if _, err := os.Stat(v.VendorDir); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
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
	return v.validateMetadataPaths()
}

func (v *Vendor) validateMetadataPaths() error {
	lockPath, err := resolvedPlacementPath(v.lockPath())
	if err != nil {
		return fmt.Errorf("assetmapper.Vendor: resolve lock path: %w", err)
	}
	importmapPath, err := resolvedPlacementPath(v.importmapPath())
	if err != nil {
		return fmt.Errorf("assetmapper.Vendor: resolve importmap path: %w", err)
	}
	transactionPath, err := resolvedPlacementPath(v.transactionPath())
	if err != nil {
		return fmt.Errorf("assetmapper.Vendor: resolve transaction path: %w", err)
	}
	paths := []struct {
		name string
		path string
	}{
		{"lock", lockPath},
		{"importmap", importmapPath},
		{"transaction journal", transactionPath},
	}
	for i := range paths {
		for j := i + 1; j < len(paths); j++ {
			if paths[i].path == paths[j].path {
				return fmt.Errorf("assetmapper.Vendor: %s and %s paths must differ", paths[i].name, paths[j].name)
			}
		}
	}
	vendorPath, err := resolvedPlacementPath(v.VendorDir)
	if err != nil {
		return fmt.Errorf("assetmapper.Vendor: resolve vendor path: %w", err)
	}
	for _, metadata := range paths {
		rel, err := filepath.Rel(vendorPath, metadata.path)
		if err != nil {
			return fmt.Errorf("assetmapper.Vendor: compare %s path: %w", metadata.name, err)
		}
		if rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("assetmapper.Vendor: %s path must be outside VendorDir", metadata.name)
		}
	}
	return nil
}

// resolvedPlacementPath resolves every existing symlinked ancestor while
// preserving a not-yet-created suffix.
func resolvedPlacementPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	probe := absolute
	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			suffix, relErr := filepath.Rel(probe, absolute)
			if relErr != nil {
				return "", relErr
			}
			if suffix == "." {
				return filepath.Clean(resolved), nil
			}
			return filepath.Clean(filepath.Join(resolved, suffix)), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", err
		}
		probe = parent
	}
}

func (v *Vendor) directRequirements() (map[string]string, error) {
	lock, exists, err := v.loadLock()
	if err != nil {
		return nil, err
	}
	direct := map[string]string{}
	if exists {
		if err := lock.Verify(v.VendorDir); err != nil {
			return nil, err
		}
		for specifier, version := range lock.DirectRequirements {
			direct[specifier] = version
		}
	}
	if len(direct) == 0 {
		// Conservative migration: preserve every legacy vendored entry as a
		// direct pin until the caller explicitly removes it.
		for specifier, entry := range v.Importmap.Entries {
			if entry.Version != "" {
				direct[specifier] = entry.Version
			}
		}
	}
	return direct, nil
}

func (v *Vendor) resolveAndReplace(ctx context.Context, direct map[string]string) error {
	requests := make([]PackageRequest, 0, len(direct))
	for specifier, version := range direct {
		requests = append(requests, PackageRequest{Name: specifier, Version: version})
	}
	sort.Slice(requests, func(i, j int) bool { return requests[i].Name < requests[j].Name })
	resolution := &Resolution{}
	var err error
	if len(requests) > 0 {
		resolution, err = v.Resolver.Resolve(ctx, requests)
		if err != nil {
			return fmt.Errorf("assetmapper.Vendor: resolve complete requirement set: %w", err)
		}
	}
	if resolution == nil {
		return fmt.Errorf("assetmapper.Vendor: resolver returned nil resolution")
	}
	resolved := make(map[string]ResolvedPackage, len(resolution.Packages))
	for _, pkg := range resolution.Packages {
		resolved[pkg.Specifier] = pkg
	}
	exactDirect := make(map[string]string, len(direct))
	for specifier, requested := range direct {
		pkg, ok := resolved[specifier]
		if !ok {
			return fmt.Errorf("assetmapper.Vendor: direct requirement %q is absent from resolved closure", specifier)
		}
		if requested != "" && pkg.Version != requested {
			return fmt.Errorf("assetmapper.Vendor: direct requirement %q resolved to %s, want %s", specifier, pkg.Version, requested)
		}
		exactDirect[specifier] = pkg.Version
	}
	return v.applyResolution(ctx, resolution, exactDirect)
}

// applyResolution publishes immutable artifacts before atomically switching
// metadata to the complete resolved closure.
func (v *Vendor) applyResolution(ctx context.Context, res *Resolution, direct map[string]string) error {
	if res == nil {
		return fmt.Errorf("assetmapper.Vendor: resolver returned nil resolution")
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

	// Fetch everything in memory before mutating disk or importmap state.
	staged, err := v.fetchAll(ctx, packages, rels, urlToSpec, limits)
	if err != nil {
		return err
	}
	lock := newVendorLock()
	lock.DirectRequirements = direct
	for _, item := range staged {
		if err := validateLockedPackage(item.pkg.Specifier, item.locked); err != nil {
			return fmt.Errorf("assetmapper.Vendor: %w", err)
		}
		lock.Packages[item.pkg.Specifier] = item.locked
	}
	if err := assignVendorOwners(lock); err != nil {
		return err
	}
	next := NewImportmap()
	for specifier, entry := range v.Importmap.Entries {
		if entry.Version == "" {
			next.Entries[specifier] = entry
		}
	}
	for _, item := range staged {
		next.Entries[item.pkg.Specifier] = ImportmapEntry{
			Path:    VendorDir + "/" + item.locked.Path,
			Version: item.pkg.Version,
			Type:    item.pkg.Type,
		}
	}
	if err := v.publishSnapshot(staged, lock, next); err != nil {
		return err
	}
	v.Importmap.Entries = next.Entries
	return nil
}

func (v *Vendor) publishSnapshot(staged []stagedPackage, lock *VendorLock, next *Importmap) error {
	if err := os.MkdirAll(v.VendorDir, 0o755); err != nil { // #nosec G301 -- served assets
		return fmt.Errorf("assetmapper.Vendor: create vendor directory: %w", err)
	}
	for _, item := range staged {
		if err := os.MkdirAll(filepath.Dir(item.dst), 0o755); err != nil { // #nosec G301 -- served assets
			return fmt.Errorf("assetmapper.Vendor: create directory for %s: %w", item.pkg.Specifier, err)
		}
		if existing, err := os.ReadFile(item.dst); err == nil { // #nosec G304 -- validated immutable vendor path
			if !bytes.Equal(existing, item.content) {
				return fmt.Errorf("assetmapper.Vendor: immutable artifact %s has different bytes", item.dst)
			}
			continue
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("assetmapper.Vendor: inspect %s: %w", item.dst, err)
		}
		if err := atomicWriteFile(item.dst, item.content, 0o644); err != nil {
			return fmt.Errorf("assetmapper.Vendor: publish %s: %w", item.pkg.Specifier, err)
		}
		if err := syncAncestorDirectories(filepath.Dir(item.dst), filepath.Dir(v.VendorDir)); err != nil {
			return fmt.Errorf("assetmapper.Vendor: sync %s: %w", item.pkg.Specifier, err)
		}
	}
	return v.publishMetadata(lock, next)
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
	knownSpecifiers := make(map[string]struct{}, len(pkgs))
	for _, pkg := range pkgs {
		knownSpecifiers[pkg.Specifier] = struct{}{}
	}

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
			dependencies := append([]string(nil), p.Dependencies...)
			dependencies = append(dependencies,
				discoverVendorDependencies(fetched.Content, p.Type, urlToSpec, knownSpecifiers)...)
			dependencies = slices.DeleteFunc(dependencies, func(dependency string) bool {
				return dependency == p.Specifier
			})
			dependencies = sortedUniqueStrings(dependencies)
			sum := sha256.Sum256(content)
			publishedRel, err := immutableVendorRelPath(p.Specifier, p.Type, hex.EncodeToString(sum[:]))
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("assetmapper.Vendor: %w", err)
					cancel()
				}
				mu.Unlock()
				return
			}
			staged[i] = stagedPackage{
				pkg:     p,
				content: content,
				dst:     filepath.Join(v.VendorDir, filepath.FromSlash(publishedRel)),
				locked: LockedPackage{
					Version:      p.Version,
					Type:         p.Type,
					Path:         publishedRel,
					SourceURL:    fetched.SourceURL,
					SourceSize:   size,
					SourceSHA256: hex.EncodeToString(sourceSum[:]),
					Size:         publishedSize,
					SHA256:       hex.EncodeToString(sum[:]),
					Dependencies: dependencies,
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

func discoverVendorDependencies(
	content []byte,
	typ string,
	urlToSpec map[string]string,
	knownSpecifiers map[string]struct{},
) []string {
	var literals []string
	add := func(literal string) {
		if dependency := urlToSpec[literal]; dependency != "" {
			literals = append(literals, dependency)
			return
		}
		if _, ok := knownSpecifiers[literal]; ok {
			literals = append(literals, literal)
		}
	}
	if typ == "css" {
		for _, ref := range extractRefs("", content, kindCSS) {
			add(ref.spec)
		}
	} else {
		for _, ref := range extractRefs("", content, kindJS) {
			add(ref.spec)
		}
	}
	return sortedUniqueStrings(literals)
}

func assignVendorOwners(lock *VendorLock) error {
	owners := make(map[string]map[string]struct{}, len(lock.Packages))
	roots := make([]string, 0, len(lock.DirectRequirements))
	for root := range lock.DirectRequirements {
		roots = append(roots, root)
	}
	sort.Strings(roots)
	for _, root := range roots {
		seen := map[string]struct{}{}
		var visit func(string) error
		visit = func(specifier string) error {
			if _, ok := seen[specifier]; ok {
				return nil
			}
			seen[specifier] = struct{}{}
			pkg, ok := lock.Packages[specifier]
			if !ok {
				return fmt.Errorf("assetmapper.Vendor: package %q depends on missing package %q", root, specifier)
			}
			if owners[specifier] == nil {
				owners[specifier] = map[string]struct{}{}
			}
			owners[specifier][root] = struct{}{}
			for _, dependency := range pkg.Dependencies {
				if err := visit(dependency); err != nil {
					return err
				}
			}
			return nil
		}
		if err := visit(root); err != nil {
			return err
		}
	}
	for specifier, pkg := range lock.Packages {
		if len(owners[specifier]) == 0 {
			return fmt.Errorf("assetmapper.Vendor: resolved package %q is not owned by any direct requirement", specifier)
		}
		pkg.Owners = make([]string, 0, len(owners[specifier]))
		for owner := range owners[specifier] {
			pkg.Owners = append(pkg.Owners, owner)
		}
		sort.Strings(pkg.Owners)
		lock.Packages[specifier] = pkg
	}
	return validateVendorGraph(lock)
}

func sortedUniqueStrings(values []string) []string {
	sort.Strings(values)
	out := values[:0]
	for _, value := range values {
		if value == "" || len(out) > 0 && out[len(out)-1] == value {
			continue
		}
		out = append(out, value)
	}
	return out
}

func immutableVendorRelPath(specifier, typ, digest string) (string, error) {
	rel, err := vendorRelPath(specifier, typ)
	if err != nil {
		return "", err
	}
	ext := filepath.Ext(rel)
	return strings.TrimSuffix(rel, ext) + "-" + digest[:HashLength] + ext, nil
}

func (v *Vendor) lockPath() string {
	if v.Lockfile != "" {
		return v.Lockfile
	}
	return filepath.Join(filepath.Dir(filepath.Clean(v.VendorDir)), VendorLockFilename)
}

func (v *Vendor) importmapPath() string {
	if v.ImportmapFile != "" {
		return v.ImportmapFile
	}
	return filepath.Join(filepath.Dir(filepath.Clean(v.VendorDir)), ImportmapFilename)
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
	for _, candidate := range scanJSImportRefs(content) {
		spec := candidate.value
		replacement, ok := urlToSpec[spec]
		if !ok {
			continue
		}
		delta := int64(len(replacement)) - int64(candidate.end-candidate.start)
		if delta > 0 && size > limit-delta {
			return nil, 0, fmt.Errorf("published package exceeds %d-byte limit", limit)
		}
		size += delta
		refs = append(refs, ref{
			spec:     spec,
			resolved: spec,
			start:    candidate.start,
			end:      candidate.end,
			kind:     referenceJSImport,
		})
	}
	if size > limit {
		return nil, 0, fmt.Errorf("published package exceeds %d-byte limit", limit)
	}
	return refs, size, nil
}
