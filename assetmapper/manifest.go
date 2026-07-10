package assetmapper

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Manifest maps logical asset paths to compiled public filenames.
//
// JSON shape (Entries' keys sorted for diff stability):
//
//	{
//	  "url_prefix": "/assets/",
//	  "entries": {
//	    "app.js": "app-7a1b2c3d.js",
//	    "images/logo.png": "images/logo-deadbeef.png"
//	  }
//	}
//
// URLPrefix captures the value [Compile] baked into rewritten references.
type Manifest struct {
	URLPrefix string            `json:"url_prefix,omitempty"`
	Entries   map[string]string `json:"entries"`
}

// ManifestFilename is the conventional file name used by [Manifest.Save]
// and [LoadManifest] inside the public asset directory.
const ManifestFilename = "manifest.json"

// NewManifest returns an empty manifest.
func NewManifest() *Manifest {
	return &Manifest{Entries: map[string]string{}}
}

// LoadManifest reads publicDir/manifest.json.
func LoadManifest(publicDir string) (*Manifest, error) {
	path := filepath.Join(publicDir, ManifestFilename)
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("assetmapper.LoadManifest: open %s: %w", path, err)
	}
	defer f.Close()
	return ParseManifest(f)
}

// ParseManifest decodes a manifest from r.
func ParseManifest(r io.Reader) (*Manifest, error) {
	var m Manifest
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		return nil, fmt.Errorf("assetmapper.ParseManifest: %w", err)
	}
	if m.Entries == nil {
		m.Entries = map[string]string{}
	}
	return &m, nil
}

// Save writes publicDir/manifest.json.
func (m *Manifest) Save(publicDir string) error {
	path := filepath.Join(publicDir, ManifestFilename)
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("assetmapper.Manifest.Save: create %s: %w", path, err)
	}
	if err := m.Write(f); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Write encodes the manifest as deterministic indented JSON.
func (m *Manifest) Write(w io.Writer) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	_, err = w.Write([]byte("\n"))
	return err
}

// Lookup returns the public file name for a logical path, or
// ("", false) if absent.
func (m *Manifest) Lookup(logicalPath string) (string, bool) {
	v, ok := m.Entries[logicalPath]
	return v, ok
}
