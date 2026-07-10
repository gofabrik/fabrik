package assetmapper

import (
	"fmt"
	"io/fs"
	"mime"
	"path"
)

// Asset returns the public URL for a logical path.
//
// Logical paths are forward-slash separated and rooted at the asset
// namespace (no leading slash). Paths with ".." segments are rejected
// as not found.
func (m *Mapper) Asset(logicalPath string) (string, error) {
	cleaned := cleanLogical(logicalPath)
	if cleaned == "" {
		return "", fmt.Errorf("%w: %q", ErrAssetNotFound, logicalPath)
	}

	if m.manifest != nil {
		// Missing manifest entries mean the caller compiled a stale asset set.
		hashedRel, ok := m.manifest.Lookup(cleaned)
		if !ok {
			return "", fmt.Errorf("%w: %s (not in manifest)", ErrAssetNotFound, logicalPath)
		}
		return m.urlPrefix + hashedRel, nil
	}

	c, err := m.loadDev(cleaned)
	if err != nil {
		return "", err
	}
	return m.urlPrefix + hashedName(cleaned, c.hash), nil
}

// loadDev resolves and caches a dev asset.
func (m *Mapper) loadDev(logicalPath string) (*cachedAsset, error) {
	if v, ok := m.devCache.Load(logicalPath); ok {
		return v.(*cachedAsset), nil
	}
	root, sub, err := m.resolveFile(logicalPath)
	if err != nil {
		return nil, err
	}
	content, err := fs.ReadFile(root.FS, sub)
	if err != nil {
		return nil, fmt.Errorf("assetmapper: read %s: %w", logicalPath, err)
	}
	c := &cachedAsset{
		content:     content,
		hash:        hashContent(content),
		contentType: contentTypeFor(logicalPath),
	}
	// Prefer a concurrent winner to keep one cached value per path.
	actual, _ := m.devCache.LoadOrStore(logicalPath, c)
	return actual.(*cachedAsset), nil
}

// contentTypeFor disables browser sniffing for unknown extensions.
func contentTypeFor(logicalPath string) string {
	if t := mime.TypeByExtension(path.Ext(logicalPath)); t != "" {
		return t
	}
	return "application/octet-stream"
}

// Invalidate drops the cached dev asset for one logical path.
func (m *Mapper) Invalidate(logicalPath string) {
	cleaned := cleanLogical(logicalPath)
	if cleaned == "" {
		return
	}
	m.devCache.Delete(cleaned)
}

// ClearCache drops every cached dev asset.
func (m *Mapper) ClearCache() {
	m.devCache.Clear()
}
