package assetmapper

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"sort"
	"strconv"
)

// RootError reports a failure attributable to one asset [Root]: a normalize
// failure, importmap discovery, a walk or read while collecting, or a stream
// hash. Index is the position in the roots slice passed to [Build] or [Check],
// so a caller merging roots it did not author - an aggregator wiring several
// packages - can map the failure back to one of them. Match it with errors.As.
type RootError struct {
	Index int
	Err   error
}

func (e *RootError) Error() string { return fmt.Sprintf("Roots[%d]: %v", e.Index, e.Err) }
func (e *RootError) Unwrap() error { return e.Err }

// BuildOption configures [Build] and [Check].
type BuildOption func(*buildConfig)

type buildConfig struct {
	urlPrefix string
}

// WithURLPrefix sets the URL prefix for hashed asset URLs.
func WithURLPrefix(prefix string) BuildOption {
	return func(c *buildConfig) { c.urlPrefix = prefix }
}

// Build compiles asset sources in memory and returns their runtime surfaces.
//
// A nil im discovers a top-level importmap.json from the roots.
//
// Build validates importmap entries before returning.
//
// Only rewritten JS and CSS are retained in memory; other files stream from their source FS.
func Build(roots []Root, im *Importmap, opts ...BuildOption) (*Compiled, error) {
	return build("assetmapper.Build", roots, im, opts)
}

// Check runs the [Build] pipeline without keeping the compiled result.
func Check(roots []Root, im *Importmap, opts ...BuildOption) error {
	_, err := build("assetmapper.Check", roots, im, opts)
	return err
}

// Compiled is the in-memory result of [Build].
//
// A Compiled is immutable after Build and safe for concurrent use.
type Compiled struct {
	mapper  *Mapper
	im      *Importmap
	entries map[string]serveEntry // hashed relative path → how to serve it
}

// serveEntry is one compiled file addressable by hashed path.
type serveEntry struct {
	logical   string
	hash      string
	size      int64
	rewritten []byte
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
	if e.rewritten != nil {
		_, _ = w.Write(e.rewritten)
		return
	}
	root, sub, err := h.c.mapper.resolveFile(e.logical)
	if err != nil {
		http.Error(w, "asset error", http.StatusInternalServerError)
		return
	}
	f, err := root.FS.Open(sub)
	if err != nil {
		http.Error(w, "asset error", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	_, _ = io.Copy(w, f)
}

// build is the shared pipeline behind [Build] and [Check]. context
// names the entry point in error messages.
func build(context string, roots []Root, im *Importmap, opts []BuildOption) (*Compiled, error) {
	roots, err := normalizeRoots(context, roots)
	if err != nil {
		return nil, err
	}
	var cfg buildConfig
	for _, o := range opts {
		o(&cfg)
	}
	prefix, err := normalizeURLPrefix(cfg.urlPrefix)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", context, err)
	}

	if im == nil {
		im, err = discoverImportmap(context, roots)
		if err != nil {
			return nil, err
		}
	}

	assets, err := collectAssets(roots)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", context, err)
	}
	if err := validateImportmap(context, im, assets); err != nil {
		return nil, err
	}

	hashedNames := make(map[string]string, len(assets))
	outputOwner := make(map[string]string, len(assets))
	entries := make(map[string]serveEntry, len(assets))

	// Pass-through assets hash independently of the dependency graph.
	var passthrough []string
	for logical, a := range assets {
		if a.kind != kindJS && a.kind != kindCSS {
			passthrough = append(passthrough, logical)
		}
	}
	sort.Strings(passthrough)
	for _, logical := range passthrough {
		a := assets[logical]
		hash, size, err := streamHash(a.root.FS, a.subPath)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", context,
				&RootError{Index: a.rootIndex, Err: fmt.Errorf("hash %s: %w", logical, err)})
		}
		hashed := hashedName(logical, hash)
		if cerr := checkCollision(context, logical, hashed, assets, outputOwner); cerr != nil {
			return nil, cerr
		}
		outputOwner[hashed] = logical
		hashedNames[logical] = hashed
		entries[hashed] = serveEntry{logical: logical, hash: hash, size: size}
	}

	// JS and CSS hash after their dependencies so rewritten URLs are final.
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
		if cerr := checkCollision(context, logical, hashed, assets, outputOwner); cerr != nil {
			return nil, cerr
		}
		outputOwner[hashed] = logical
		hashedNames[logical] = hashed
		e := serveEntry{logical: logical, hash: hash, size: int64(len(rewritten))}
		// Retain bytes only when rewriting changed them; unchanged
		// JS / CSS serves from the source FS like any pass-through.
		if !bytes.Equal(rewritten, a.content) {
			e.rewritten = rewritten
		}
		entries[hashed] = e
	}

	manifest := NewManifest()
	manifest.URLPrefix = prefix
	for logical, hashed := range hashedNames {
		manifest.Entries[logical] = hashed
	}
	return &Compiled{
		mapper:  &Mapper{roots: roots, urlPrefix: prefix, manifest: manifest},
		im:      im,
		entries: entries,
	}, nil
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
			return nil, fmt.Errorf("%s: %w", context,
				&RootError{Index: i, Err: fmt.Errorf("open %s: %w", ImportmapFilename, err)})
		}
		if found >= 0 {
			_ = f.Close()
			return nil, fmt.Errorf("%s: %s found in Roots[%d] and Roots[%d]; only one root may carry it", context, ImportmapFilename, found, i)
		}
		im, err = ParseImportmap(f)
		_ = f.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", context, &RootError{Index: i, Err: err})
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
		if e.Path != "" && e.Version != "" {
			return fmt.Errorf("%s: importmap entry %q has both \"path\" and \"version\"; pick one", context, k)
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

// streamHash hashes a file's content without retaining it, returning
// the truncated digest and the byte count.
func streamHash(fsys fs.FS, p string) (hash string, size int64, err error) {
	f, err := fsys.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil))[:HashLength], n, nil
}
