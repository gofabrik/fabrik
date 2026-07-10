package assetmapper

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"
	"sync"
)

// ImportmapFilename is the conventional file name for an asset tree importmap.
const ImportmapFilename = "importmap.json"

// Importmap maps browser bare specifiers to assets.
//
// Entries are one of two shapes:
//
//   - Local: Path set, Version empty. Resolved through [Mapper.Asset]
//     so it tracks dev/prod hashing automatically.
//   - Vendored: Version set, Path empty. Resolved against the
//     convention path vendor/<key>.js (or .css). Vendored files are
//     downloaded by [Vendor.Require]; they live under the mapper's
//     asset roots like any other file.
//
// Importmap can be loaded from disk, edited by [Vendor], and rendered into HTML.
type Importmap struct {
	Entries map[string]ImportmapEntry

	// preloadCache is used only in prod mode, where source files do not change at runtime.
	preloadCache sync.Map // map[preloadCacheKey]preloadResult
}

// preloadCacheKey identifies one cached preload graph result.
type preloadCacheKey struct {
	mapper      *Mapper
	entrypoints string
}

// ImportmapEntry is one bare-specifier mapping.
type ImportmapEntry struct {
	// Path is the logical asset path for local entries. Mutually
	// exclusive with Version.
	Path string `json:"path,omitempty"`
	// Version is the package version for vendored entries. Mutually
	// exclusive with Path.
	Version string `json:"version,omitempty"`
	// Type is "js" (default) or "css". Affects how Render emits the
	// entrypoint tag:
	//
	//   - js  → <script type="module">import "name";</script>
	//   - css → <link rel="stylesheet" href="...">
	//
	// Type also controls the conventional file extension when
	// resolving Vendored entries (vendor/<key>.css vs vendor/<key>.js).
	Type string `json:"type,omitempty"`
	// Entrypoint, when true, makes the entry eligible to be passed
	// by name to [Importmap.Render]. Non-entrypoint entries appear
	// in the importmap (so JS imports can reach them) but cannot be
	// requested as page entrypoints.
	Entrypoint bool `json:"entrypoint,omitempty"`
}

// NewImportmap returns an empty importmap.
func NewImportmap() *Importmap {
	return &Importmap{Entries: map[string]ImportmapEntry{}}
}

// LoadImportmap reads an importmap from path.
func LoadImportmap(path string) (*Importmap, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("assetmapper.LoadImportmap: open %s: %w", path, err)
	}
	defer f.Close()
	return ParseImportmap(f)
}

// ParseImportmap decodes an importmap and rejects unknown JSON fields.
func ParseImportmap(r io.Reader) (*Importmap, error) {
	var entries map[string]ImportmapEntry
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&entries); err != nil {
		return nil, fmt.Errorf("assetmapper.ParseImportmap: %w", err)
	}
	if entries == nil {
		entries = map[string]ImportmapEntry{}
	}
	return &Importmap{Entries: entries}, nil
}

// Save writes the importmap to path with sorted keys and two-space
// indentation. The directory must already exist.
func (im *Importmap) Save(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("assetmapper.Importmap.Save: create %s: %w", path, err)
	}
	if err := im.Write(f); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Write encodes the importmap as deterministic indented JSON.
func (im *Importmap) Write(w io.Writer) error {
	data, err := json.MarshalIndent(im.Entries, "", "  ")
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	_, err = w.Write([]byte("\n"))
	return err
}

// RenderOptions controls importmap and preload tag rendering.
type RenderOptions struct {
	// Entrypoints names the importmap entries that should produce
	// page entrypoint tags (and seed the modulepreload graph walk
	// for [Importmap.RenderWithOptions]). Empty means importmap-only
	// output.
	Entrypoints []string

	// Nonce, when non-empty, is set on every emitted <script> and <link> tag.
	Nonce string
}

// Render renders importmap, preload, stylesheet, and entrypoint tags.
func (im *Importmap) Render(m *Mapper, entrypoints ...string) (string, error) {
	return im.RenderWithOptions(m, RenderOptions{Entrypoints: entrypoints})
}

// RenderWithOptions returns the importmap HTML for a page <head>.
//
// Output order:
//
//  1. <script type="importmap">{...}</script>, every entry resolved
//     to its public URL via mapper.
//  2. <link rel="modulepreload"> tags, one per JS module
//     transitively reachable from any JS entrypoint, so the browser
//     can begin fetching deps in parallel with the importmap parse.
//  3. <link rel="preload" as="style"> tags, one per CSS file
//     reached via `import "./x.css"` from JS. CSS entrypoints get
//     the full stylesheet link in step 4 instead.
//  4. <link rel="stylesheet"> tags, one per CSS entrypoint.
//  5. <script type="module">import "name";</script> tags, one per
//     JS entrypoint. The browser uses the importmap from step 1 to
//     resolve the bare specifier to a URL (already cached thanks to
//     step 2).
//
// Use [FuncMap] for html/template helpers that return [template.HTML].
func (im *Importmap) RenderWithOptions(m *Mapper, opts RenderOptions) (string, error) {
	if m == nil {
		return "", fmt.Errorf("assetmapper.Importmap.Render: nil Mapper")
	}
	if err := im.validateEntrypoints(opts.Entrypoints); err != nil {
		return "", fmt.Errorf("assetmapper.Importmap.Render: %w", err)
	}

	keys := make([]string, 0, len(im.Entries))
	for k := range im.Entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	resolved := make(map[string]string, len(keys))
	for _, k := range keys {
		url, err := im.resolveEntry(m, k, im.Entries[k])
		if err != nil {
			return "", fmt.Errorf("assetmapper.Importmap.Render: resolve %q: %w", k, err)
		}
		resolved[k] = url
	}

	nonce := nonceAttr(opts.Nonce)

	var out strings.Builder
	// json.Marshal escapes < > & by default, so embedding inside a
	// <script> tag is safe even for URLs that include those chars.
	out.WriteString(`<script type="importmap"`)
	out.WriteString(nonce)
	out.WriteString(`>{"imports":{`)
	for i, k := range keys {
		if i > 0 {
			out.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		vb, _ := json.Marshal(resolved[k])
		out.Write(kb)
		out.WriteByte(':')
		out.Write(vb)
	}
	out.WriteString("}}</script>")

	preloads, err := im.preloadGraph(m, opts.Entrypoints)
	if err != nil {
		return "", fmt.Errorf("assetmapper.Importmap.Render: build preload graph: %w", err)
	}
	for _, url := range preloads.JSURLs {
		out.WriteString("\n")
		out.WriteString(`<link rel="modulepreload" href="`)
		out.WriteString(html.EscapeString(url))
		out.WriteString(`"`)
		out.WriteString(nonce)
		out.WriteString(">")
	}
	// CSS reached from JS imports gets a preload hint, not a stylesheet tag.
	for _, url := range preloads.CSSURLs {
		out.WriteString("\n")
		out.WriteString(`<link rel="preload" as="style" href="`)
		out.WriteString(html.EscapeString(url))
		out.WriteString(`"`)
		out.WriteString(nonce)
		out.WriteString(">")
	}

	for _, name := range opts.Entrypoints {
		out.WriteString("\n")
		entry := im.Entries[name]
		switch entry.Type {
		case "css":
			out.WriteString(`<link rel="stylesheet" href="`)
			out.WriteString(html.EscapeString(resolved[name]))
			out.WriteString(`"`)
			out.WriteString(nonce)
			out.WriteString(">")
		default:
			out.WriteString(`<script type="module"`)
			out.WriteString(nonce)
			out.WriteString(">import ")
			nb, _ := json.Marshal(name)
			out.Write(nb)
			out.WriteString(";</script>")
		}
	}
	return out.String(), nil
}

// ModulePreloadLinks renders JS modulepreload tags.
func (im *Importmap) ModulePreloadLinks(m *Mapper, entrypoints ...string) (string, error) {
	return im.ModulePreloadLinksWithOptions(m, RenderOptions{Entrypoints: entrypoints})
}

// ModulePreloadLinksWithOptions returns the JS modulepreload subset of [Importmap.RenderWithOptions].
//
// CSS is excluded because modulepreload applies only to JavaScript modules.
func (im *Importmap) ModulePreloadLinksWithOptions(m *Mapper, opts RenderOptions) (string, error) {
	if m == nil {
		return "", fmt.Errorf("assetmapper.Importmap.ModulePreloadLinks: nil Mapper")
	}
	if err := im.validateEntrypoints(opts.Entrypoints); err != nil {
		return "", fmt.Errorf("assetmapper.Importmap.ModulePreloadLinks: %w", err)
	}
	result, err := im.preloadGraph(m, opts.Entrypoints)
	if err != nil {
		return "", fmt.Errorf("assetmapper.Importmap.ModulePreloadLinks: %w", err)
	}
	nonce := nonceAttr(opts.Nonce)
	var out strings.Builder
	for i, url := range result.JSURLs {
		if i > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(`<link rel="modulepreload" href="`)
		out.WriteString(html.EscapeString(url))
		out.WriteString(`"`)
		out.WriteString(nonce)
		out.WriteString(">")
	}
	return out.String(), nil
}

// nonceAttr returns an escaped nonce attribute, including the leading space.
func nonceAttr(nonce string) string {
	if nonce == "" {
		return ""
	}
	return ` nonce="` + html.EscapeString(nonce) + `"`
}

// validateEntrypoints checks that every requested entrypoint is renderable.
func (im *Importmap) validateEntrypoints(entrypoints []string) error {
	for _, name := range entrypoints {
		entry, ok := im.Entries[name]
		if !ok {
			return fmt.Errorf("entrypoint %q not in importmap", name)
		}
		if !entry.Entrypoint {
			return fmt.Errorf("entry %q is not marked as entrypoint (set \"entrypoint\": true in importmap.json)", name)
		}
	}
	return nil
}

// preloadResult is the partitioned output of a preload graph walk.
type preloadResult struct {
	JSURLs  []string
	CSSURLs []string
}

// preloadGraph walks JS entrypoints and returns deterministic preload URLs.
//
// Prod mode memoises results; dev mode re-walks so edits surface immediately.
func (im *Importmap) preloadGraph(m *Mapper, entrypoints []string) (preloadResult, error) {
	if m.manifest != nil {
		key := preloadCacheKey{mapper: m, entrypoints: strings.Join(entrypoints, "\x00")}
		if v, ok := im.preloadCache.Load(key); ok {
			return v.(preloadResult), nil
		}
		result, err := im.computePreloadGraph(m, entrypoints)
		if err != nil {
			return preloadResult{}, err
		}
		// Concurrent misses compute the same value.
		im.preloadCache.Store(key, result)
		return result, nil
	}
	return im.computePreloadGraph(m, entrypoints)
}

// computePreloadGraph is the uncached preload walker.
func (im *Importmap) computePreloadGraph(m *Mapper, entrypoints []string) (preloadResult, error) {
	var js, css []string
	seen := map[string]bool{}

	var visit func(logical string) error
	visit = func(logical string) error {
		if logical == "" || seen[logical] {
			return nil
		}
		seen[logical] = true

		kind := kindOf(logical)
		if kind != kindJS && kind != kindCSS {
			// Non-JS/CSS assets are discovered by the browser from the referrer.
			return nil
		}

		url, err := m.Asset(logical)
		if err != nil {
			return fmt.Errorf("resolve URL for %q: %w", logical, err)
		}

		if kind == kindCSS {
			// Avoid recursing through CSS; the extra head tags rarely pay off.
			css = append(css, url)
			return nil
		}

		js = append(js, url)

		// Best-effort source read for transitive deps.
		root, sub, err := m.resolveFile(logical)
		if err != nil {
			return nil
		}
		content, err := fs.ReadFile(root.FS, sub)
		if err != nil {
			return nil
		}

		for _, r := range extractRefs(logical, content, kindJS) {
			if r.resolved != "" {
				if err := visit(r.resolved); err != nil {
					return err
				}
				continue
			}
			// Bare specifier: try the importmap.
			entry, ok := im.Entries[r.spec]
			if !ok {
				continue
			}
			if err := visit(logicalForEntry(r.spec, entry)); err != nil {
				return err
			}
		}
		return nil
	}

	for _, name := range entrypoints {
		entry := im.Entries[name]
		if entry.Type == "css" {
			continue
		}
		if err := visit(logicalForEntry(name, entry)); err != nil {
			return preloadResult{}, err
		}
	}
	return preloadResult{JSURLs: js, CSSURLs: css}, nil
}

// logicalForEntry returns the logical asset path backing an importmap entry.
func logicalForEntry(spec string, entry ImportmapEntry) string {
	if entry.Path != "" {
		return entry.Path
	}
	if entry.Version == "" {
		return ""
	}
	ext := ".js"
	if entry.Type == "css" {
		ext = ".css"
	}
	return "vendor/" + spec + ext
}

// resolveEntry turns one importmap entry into a public URL.
func (im *Importmap) resolveEntry(m *Mapper, key string, entry ImportmapEntry) (string, error) {
	if entry.Path == "" && entry.Version == "" {
		return "", fmt.Errorf("entry has neither \"path\" (local) nor \"version\" (vendored)")
	}
	if entry.Path != "" && entry.Version != "" {
		return "", fmt.Errorf("entry has both \"path\" and \"version\"; pick one")
	}
	if entry.Path != "" {
		return m.Asset(entry.Path)
	}
	ext := ".js"
	if entry.Type == "css" {
		ext = ".css"
	}
	return m.Asset("vendor/" + key + ext)
}
