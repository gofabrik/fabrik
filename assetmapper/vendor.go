package assetmapper

import (
	"context"
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

// PackageResolver resolves package requests and downloads package files.
type PackageResolver interface {
	Resolve(ctx context.Context, reqs []PackageRequest) (*Resolution, error)
	Fetch(ctx context.Context, url string) ([]byte, error)
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
	for spec, entry := range v.Importmap.Entries {
		if entry.Version == "" {
			continue // local entries don't live under VendorDir
		}
		ext := ".js"
		if entry.Type == "css" {
			ext = ".css"
		}
		expected[filepath.FromSlash(spec+ext)] = struct{}{}
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
	if len(res.Packages) == 0 {
		return nil
	}

	// Validate every destination path before network or disk work.
	rels := make([]string, len(res.Packages))
	for i, p := range res.Packages {
		rel, err := vendorRelPath(p.Specifier, p.Type)
		if err != nil {
			return fmt.Errorf("assetmapper.Vendor: %w", err)
		}
		rels[i] = rel
	}

	urlToSpec := make(map[string]string, len(res.Packages))
	for _, p := range res.Packages {
		urlToSpec[p.URL] = p.Specifier
	}

	// Fetch everything in memory before mutating disk or importmap state.
	staged, err := v.fetchAll(ctx, res.Packages, rels, urlToSpec)
	if err != nil {
		return err
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
}

// fetchAll downloads packages in parallel and cancels remaining work on the first error.
func (v *Vendor) fetchAll(ctx context.Context, pkgs []ResolvedPackage, rels []string, urlToSpec map[string]string) ([]stagedPackage, error) {
	staged := make([]stagedPackage, len(pkgs))

	derived, cancel := context.WithCancel(ctx)
	defer cancel()

	sem := make(chan struct{}, vendorDownloadConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

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

			content, err := v.Resolver.Fetch(derived, p.URL)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("assetmapper.Vendor: fetch %s: %w", p.URL, err)
					cancel()
				}
				mu.Unlock()
				return
			}
			if strings.HasSuffix(rels[i], ".js") {
				content = rewriteVendoredJS(content, urlToSpec)
			}
			staged[i] = stagedPackage{
				pkg:     p,
				content: content,
				dst:     filepath.Join(v.VendorDir, filepath.FromSlash(rels[i])),
			}
		}(i, p)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return staged, nil
}

// rewriteVendoredJS replaces known upstream URLs with their bare specifiers.
func rewriteVendoredJS(content []byte, urlToSpec map[string]string) []byte {
	var refs []ref
	for _, m := range jsImportRE.FindAllSubmatchIndex(content, -1) {
		spec := string(content[m[2]:m[3]])
		resolved := ""
		if _, ok := urlToSpec[spec]; ok {
			resolved = spec
		}
		refs = append(refs, ref{
			spec:     spec,
			resolved: resolved,
			start:    m[2],
			end:      m[3],
		})
	}
	return rewriteRefs(content, refs, func(r ref) string {
		return urlToSpec[r.spec]
	})
}
