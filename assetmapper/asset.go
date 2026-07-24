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

// loadDev reads the source on every call so edits appear without a restart.
func (m *Mapper) loadDev(logicalPath string) (*devAsset, error) {
	root, sub, err := m.resolveFile(logicalPath)
	if err != nil {
		return nil, err
	}
	content, err := fs.ReadFile(root.FS, sub)
	if err != nil {
		return nil, fmt.Errorf("assetmapper: read %s: %w", logicalPath, err)
	}
	return &devAsset{
		content:     content,
		hash:        hashContent(content),
		contentType: contentTypeFor(logicalPath),
	}, nil
}

// contentTypeFor disables browser sniffing for unknown extensions.
func contentTypeFor(logicalPath string) string {
	if t := mime.TypeByExtension(path.Ext(logicalPath)); t != "" {
		return t
	}
	return "application/octet-stream"
}
