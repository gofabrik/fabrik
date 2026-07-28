package assetmapper

import (
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// BuildOption configures [Build], [Check], and [NewSource].
type BuildOption func(*buildConfig)

type buildConfig struct {
	urlPrefix       string
	strictAssetURLs bool
}

// WithURLPrefix sets the URL prefix for hashed asset URLs.
func WithURLPrefix(prefix string) BuildOption {
	return func(c *buildConfig) { c.urlPrefix = prefix }
}

// WithStrictAssetURLs makes unresolved root-relative references and CSS
// url(...) references build errors. Relative JS imports and CSS imports are
// always strict; bare JS imports must always exist in the importmap.
func WithStrictAssetURLs() BuildOption {
	return func(c *buildConfig) { c.strictAssetURLs = true }
}

// Build compiles asset sources in memory and returns their runtime surfaces.
//
// A nil im discovers a top-level importmap.json from the roots.
//
// Build validates importmap entries before returning.
//
// Build snapshots every served byte. Source filesystem mutations after Build
// cannot change content served under an immutable hashed URL.
func Build(roots []Root, im *Importmap, opts ...BuildOption) (*Compiled, error) {
	plan, err := planBuild("assetmapper.Build", roots, im, opts)
	if err != nil {
		return nil, err
	}
	entries := make(map[string]serveEntry, len(plan.assets))
	for _, logical := range plan.order {
		asset := plan.assets[logical]
		entry := serveEntry{
			logical: asset.logical,
			hash:    asset.hash,
			size:    asset.size,
		}
		if asset.kind == kindJS || asset.kind == kindCSS {
			entry.content = asset.content
		} else {
			content, err := fs.ReadFile(asset.root.FS, asset.subPath)
			if err != nil {
				return nil, fmt.Errorf("assetmapper.Build: snapshot %s: %w", logical, err)
			}
			if int64(len(content)) != asset.size || hashContent(content) != asset.hash {
				return nil, fmt.Errorf("assetmapper.Build: source %s changed while snapshotting", logical)
			}
			entry.content = content
		}
		entries[asset.output] = entry
	}
	return &Compiled{
		mapper:           plan.mapper(),
		im:               plan.importmap,
		entries:          entries,
		cspImportmapHash: plan.cspImportmapHash,
	}, nil
}

// Check runs the [Build] pipeline without keeping the compiled result.
func Check(roots []Root, im *Importmap, opts ...BuildOption) error {
	_, err := planBuild("assetmapper.Check", roots, im, opts)
	return err
}

// Compiled is the in-memory result of [Build].
//
// A Compiled is safe for concurrent use and independent of its source
// filesystems: [Build] fixes importmap rendering and snapshots all served bytes.
type Compiled struct {
	mapper           *Mapper
	im               *Importmap
	entries          map[string]serveEntry // hashed relative path → how to serve it
	cspImportmapHash string
}

// serveEntry is one compiled file addressable by hashed path.
type serveEntry struct {
	logical string
	hash    string
	size    int64
	content []byte
}

// Asset returns the public URL for a logical path.
func (c *Compiled) Asset(logical string) (string, error) {
	return c.mapper.Asset(logical)
}

// FuncMap returns the template helpers bound to this compiled result;
// see [FuncMap] for the helper reference.
func (c *Compiled) FuncMap() template.FuncMap {
	return FuncMap(c.mapper, c.im)
}

// CSPImportmapHash returns the Content-Security-Policy hash source,
// "'sha256-<base64>'", for the one inline script the importmap helpers
// emit. The body is independent of entrypoint selection and nonce, and
// Build freezes it, so the value holds for the life of the Compiled.
func (c *Compiled) CSPImportmapHash() string {
	return c.cspImportmapHash
}

// ImportmapCSPSources returns the stable CSP source for compiled assets.
func (c *Compiled) ImportmapCSPSources() []string {
	return []string{c.cspImportmapHash}
}

// URLPrefix returns the resolved asset URL prefix.
func (c *Compiled) URLPrefix() string { return c.mapper.urlPrefix }

// Handler serves compiled assets at their hashed URLs.
//
//	mux.Handle("/assets/", compiled.Handler())
//
// Register it directly under the URL prefix, with no [http.StripPrefix] wrapper.
func (c *Compiled) Handler() http.Handler {
	return &compiledHandler{c: c}
}

type compiledHandler struct {
	c *Compiled
}

func (h *compiledHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	prefix := h.c.mapper.urlPrefix
	if len(r.URL.Path) <= len(prefix) || r.URL.Path[:len(prefix)] != prefix {
		http.NotFound(w, r)
		return
	}
	e, ok := h.c.entries[r.URL.Path[len(prefix):]]
	if !ok {
		http.NotFound(w, r)
		return
	}

	etag := `"` + e.hash + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", contentTypeFor(e.logical))
	w.Header().Set("Content-Length", strconv.FormatInt(e.size, 10))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(e.content)
}

// discoverImportmap implements the nil-im contract of [Build]: read
// importmap.json from the top of each root's tree, allowing at most
// one across all roots.
func discoverImportmap(context string, roots []Root) (*Importmap, error) {
	found := -1
	var im *Importmap
	for i, r := range roots {
		f, err := r.FS.Open(ImportmapFilename)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("%s: Roots[%d]: open %s: %w", context, i, ImportmapFilename, err)
		}
		if found >= 0 {
			_ = f.Close()
			return nil, fmt.Errorf("%s: %s found in Roots[%d] and Roots[%d]; only one root may carry it", context, ImportmapFilename, found, i)
		}
		im, err = ParseImportmap(f)
		_ = f.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: Roots[%d]: %w", context, i, err)
		}
		found = i
	}
	if im == nil {
		im = NewImportmap()
	}
	return im, nil
}

// validateImportmap checks every entry against the compiled asset
// set, in sorted key order so the reported error is deterministic.
func validateImportmap(context string, im *Importmap, assets map[string]*collectedAsset) error {
	keys := make([]string, 0, len(im.Entries))
	for k := range im.Entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		e := im.Entries[k]
		if e.Path == "" && e.Version == "" {
			return fmt.Errorf("%s: importmap entry %q has neither \"path\" (local) nor \"version\" (vendored)", context, k)
		}
		if e.Path != "" && e.Version != "" && !strings.HasPrefix(e.Path, VendorDir+"/") {
			return fmt.Errorf("%s: importmap entry %q has both \"path\" and \"version\"; vendored paths must be under %s/", context, k, VendorDir)
		}
		switch e.Type {
		case "", "js", "css":
		default:
			return fmt.Errorf("%s: importmap entry %q has invalid type %q (want \"js\" or \"css\")", context, k, e.Type)
		}
		logical := cleanLogical(logicalForEntry(k, e))
		if _, ok := assets[logical]; !ok {
			return fmt.Errorf("%s: importmap entry %q resolves to %q, which is not a known asset", context, k, logicalForEntry(k, e))
		}
	}
	return nil
}
