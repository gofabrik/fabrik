// Package assetmapper maps logical asset paths to content-hashed public URLs.
//
// Surface:
//
//   - [Build] compiles embedded sources in memory at startup and
//     returns a [Compiled]: an [http.Handler] for the hashed URLs, an
//     html/template [FuncMap], and programmatic URL lookup. [Check]
//     runs the same pipeline without producing bytes, for CI.
//   - [Mapper] resolves logical paths to public URLs; in dev mode it
//     serves sources directly via [Mapper.Handler] with lazy hashing.
//   - [Compile] materializes the hashed tree to a directory and
//     writes a manifest, for CDN workflows that want files on disk.
//   - [Importmap] renders the browser's importmap, modulepreload
//     links, and entrypoint script tags.
//   - [Vendor] downloads JS packages via jspm.io into assets/vendor
//     and registers them in the importmap.
//
// The HTTP and template surfaces use plain [http.Handler] and html/template helpers.
package assetmapper

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"sync"
)

// ErrAssetNotFound reports an unknown logical asset path.
var ErrAssetNotFound = errors.New("asset not found")

// Root binds an [fs.FS] into the logical asset namespace.
//
// Multiple roots are searched in order; the first match wins.
//
// Dir selects a subdirectory inside FS as the root's tree. An
// [embed.FS] built from `//go:embed all:assets` carries the "assets"
// prefix on every path; Dir strips it without an fs.Sub chain at the
// call site:
//
//	//go:embed all:assets
//	var Assets embed.FS
//
//	Root{FS: Assets, Dir: "assets"}  // files appear as "foo.css", not "assets/foo.css"
//
// Dir must be a clean relative path: no leading or trailing slash, no
// "." or ".." segments. Empty Dir uses the whole FS.
//
// MountAt prefixes every file path in this root with the given segment.
//
//	Root{FS: jobsAssets, Dir: "assets", MountAt: "jobs"}  // "jobs/foo.css" reads "assets/foo.css"
//
// Empty MountAt mounts the root directly at the logical namespace
// root.
type Root struct {
	FS      fs.FS
	Dir     string
	MountAt string
}

// normalizeRoots validates roots and applies Dir with [fs.Sub].
func normalizeRoots(context string, roots []Root) ([]Root, error) {
	if len(roots) == 0 {
		return nil, fmt.Errorf("%s: at least one Root is required", context)
	}
	out := make([]Root, len(roots))
	for i, r := range roots {
		if r.FS == nil {
			return nil, fmt.Errorf("%s: %w", context, &RootError{Index: i, Err: errors.New("FS is nil")})
		}
		if err := validateMount(r.Dir); err != nil {
			return nil, fmt.Errorf("%s: %w", context, &RootError{Index: i, Err: fmt.Errorf("Dir: %w", err)})
		}
		if err := validateMount(r.MountAt); err != nil {
			return nil, fmt.Errorf("%s: %w", context, &RootError{Index: i, Err: fmt.Errorf("MountAt: %w", err)})
		}
		if r.Dir != "" {
			sub, err := fs.Sub(r.FS, r.Dir)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", context, &RootError{Index: i, Err: fmt.Errorf("Dir %q: %w", r.Dir, err)})
			}
			r.FS = sub
			r.Dir = ""
		}
		out[i] = r
	}
	return out, nil
}

// Config controls [New].
type Config struct {
	Roots     []Root
	URLPrefix string
	Manifest  *Manifest
}

// Mapper resolves asset URLs and serves dev assets.
//
// A Mapper is safe for concurrent use.
type Mapper struct {
	roots     []Root
	urlPrefix string
	manifest  *Manifest // nil = dev mode

	// devCache memoises source reads and hashes until restart or ClearCache.
	devCache sync.Map // map[string]*cachedAsset
}

type cachedAsset struct {
	content     []byte
	hash        string
	contentType string
}

// New constructs a Mapper.
func New(cfg Config) (*Mapper, error) {
	roots, err := normalizeRoots("assetmapper.New", cfg.Roots)
	if err != nil {
		return nil, err
	}
	prefix, err := normalizeURLPrefix(cfg.URLPrefix)
	if err != nil {
		return nil, fmt.Errorf("assetmapper.New: %w", err)
	}
	// A manifest and runtime mapper must agree on the URL prefix.
	if cfg.Manifest != nil && cfg.Manifest.URLPrefix != "" {
		manifestPrefix, err := normalizeURLPrefix(cfg.Manifest.URLPrefix)
		if err != nil {
			return nil, fmt.Errorf("assetmapper.New: manifest URLPrefix invalid: %w", err)
		}
		if manifestPrefix != prefix {
			return nil, fmt.Errorf("assetmapper.New: URLPrefix mismatch: Config %q vs Manifest %q (the manifest was compiled with a different prefix — recompile or set Config.URLPrefix to match)", prefix, manifestPrefix)
		}
	}
	return &Mapper{
		roots:     roots,
		urlPrefix: prefix,
		manifest:  cfg.Manifest,
	}, nil
}

// normalizeURLPrefix applies the shared asset-prefix rules.
func normalizeURLPrefix(p string) (string, error) {
	if p == "" {
		p = "/assets/"
	}
	if !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("URLPrefix %q must start with /", p)
	}
	cleaned := path.Clean(p)
	if !strings.HasSuffix(cleaned, "/") {
		cleaned += "/"
	}
	return cleaned, nil
}

// URLPrefix returns the resolved URL prefix.
func (m *Mapper) URLPrefix() string { return m.urlPrefix }

// resolveFile locates a logical path in the configured roots.
func (m *Mapper) resolveFile(logicalPath string) (Root, string, error) {
	cleaned := cleanLogical(logicalPath)
	if cleaned == "" {
		return Root{}, "", fmt.Errorf("%w: empty logical path", ErrAssetNotFound)
	}
	for _, r := range m.roots {
		sub := cleaned
		if r.MountAt != "" {
			if !strings.HasPrefix(cleaned, r.MountAt+"/") && cleaned != r.MountAt {
				continue
			}
			sub = strings.TrimPrefix(cleaned, r.MountAt)
			sub = strings.TrimPrefix(sub, "/")
			if sub == "" {
				// The mount root itself is not an asset.
				continue
			}
		}
		// Top-level importmap.json is configuration, not an asset.
		if sub == ImportmapFilename {
			continue
		}
		if _, err := fs.Stat(r.FS, sub); err == nil {
			return r, sub, nil
		}
	}
	return Root{}, "", fmt.Errorf("%w: %s", ErrAssetNotFound, logicalPath)
}

// cleanLogical normalises a logical path: forward slashes, no leading
// slash, no "." or ".." segments. Returns "" for an invalid path.
func cleanLogical(p string) string {
	if p == "" {
		return ""
	}
	p = strings.TrimPrefix(p, "/")
	cleaned := path.Clean(p)
	if cleaned == "." || strings.HasPrefix(cleaned, "../") || cleaned == ".." {
		return ""
	}
	return cleaned
}

// validateMount checks that a MountAt prefix is a forward-slash path
// with no leading / trailing slash, no traversal ("." or ".."), and
// no empty segments. Empty input is valid (mount-at-root). Returns a
// human-readable error describing what's wrong.
func validateMount(m string) error {
	if m == "" {
		return nil
	}
	if strings.HasPrefix(m, "/") {
		return fmt.Errorf("%q has a leading slash", m)
	}
	if strings.HasSuffix(m, "/") {
		return fmt.Errorf("%q has a trailing slash", m)
	}
	for _, seg := range strings.Split(m, "/") {
		switch seg {
		case "":
			return fmt.Errorf("%q has an empty segment", m)
		case ".", "..":
			return fmt.Errorf("%q has a %q segment", m, seg)
		}
	}
	if path.Clean(m) != m {
		return fmt.Errorf("%q is not normalised (path.Clean would change it)", m)
	}
	return nil
}
