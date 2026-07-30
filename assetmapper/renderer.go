package assetmapper

// ImportmapRenderer binds a Mapper to an immutable Importmap snapshot.
//
// A renderer and its template helpers are safe for concurrent use. Mutating
// the Importmap builder used to create it does not change rendered output or
// invalidate its preload cache.
type ImportmapRenderer struct {
	mapper    *Mapper
	importmap *Importmap
}

// Bind snapshots im and binds the snapshot to m for runtime rendering.
//
// Importmap is a mutable builder, so callers must not mutate Entries
// concurrently with Bind itself. Once Bind returns, the renderer is independent
// of the builder and safe for concurrent use.
func (im *Importmap) Bind(m *Mapper) *ImportmapRenderer {
	return &ImportmapRenderer{
		mapper:    m,
		importmap: snapshotImportmap(im),
	}
}

func snapshotImportmap(src *Importmap) *Importmap {
	dst := NewImportmap()
	if src == nil {
		return dst
	}
	for key, entry := range src.Entries {
		dst.Entries[key] = entry
	}
	return dst
}

// Render renders importmap, preload, stylesheet, and entrypoint tags from the
// bound snapshot.
func (r *ImportmapRenderer) Render(entrypoints ...string) (string, error) {
	return r.RenderWithOptions(RenderOptions{Entrypoints: entrypoints})
}

// RenderWithOptions renders a page head from the bound snapshot.
func (r *ImportmapRenderer) RenderWithOptions(opts RenderOptions) (string, error) {
	return r.importmap.render("assetmapper.Importmap.Render", r.mapper, opts)
}

// ModulePreloadLinks renders JavaScript modulepreload tags from the bound
// snapshot.
func (r *ImportmapRenderer) ModulePreloadLinks(entrypoints ...string) (string, error) {
	return r.ModulePreloadLinksWithOptions(RenderOptions{Entrypoints: entrypoints})
}

// ModulePreloadLinksWithOptions renders JavaScript modulepreload tags from the
// bound snapshot.
func (r *ImportmapRenderer) ModulePreloadLinksWithOptions(opts RenderOptions) (string, error) {
	return r.importmap.modulePreloadLinksWithOptions(r.mapper, opts)
}
