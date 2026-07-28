package assetmapper

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Manifest maps logical asset paths to compiled public filenames.
//
// JSON shape (Entries' keys sorted for diff stability):
//
//	{
//	  "url_prefix": "/assets/",
//	  "entries": {
//	    "app.js": "app-7a1b2c3d4e5f60718293.js",
//	    "images/logo.png": "images/logo-deadbeef0123456789ab.png"
//	  },
//	  "dependencies": {
//	    "app.js": ["shared.js"]
//	  }
//	}
//
// URLPrefix captures the value [Compile] baked into rewritten references.
type Manifest struct {
	URLPrefix string            `json:"url_prefix,omitempty"`
	Entries   map[string]string `json:"entries"`
	// Dependencies records the validated logical asset graph used for
	// production preload rendering.
	Dependencies map[string][]string `json:"dependencies,omitempty"`
}

// ManifestFilename is the conventional file name used by [Manifest.Save]
// and [LoadManifest] inside the public asset directory.
const ManifestFilename = "manifest.json"

// NewManifest returns an empty manifest.
func NewManifest() *Manifest {
	return &Manifest{Entries: map[string]string{}}
}

func snapshotManifest(src *Manifest) (*Manifest, error) {
	if src == nil {
		return nil, nil
	}
	dst := &Manifest{
		URLPrefix:    src.URLPrefix,
		Entries:      make(map[string]string, len(src.Entries)),
		Dependencies: nil,
	}
	for logical, output := range src.Entries {
		if err := validateManifestPath(logical); err != nil {
			return nil, fmt.Errorf("manifest logical path %q: %w", logical, err)
		}
		if err := validateManifestPath(output); err != nil {
			return nil, fmt.Errorf("manifest output path for %q (%q): %w", logical, output, err)
		}
		dst.Entries[logical] = output
	}
	if src.Dependencies != nil {
		dst.Dependencies = make(map[string][]string, len(src.Dependencies))
		for logical, dependencies := range src.Dependencies {
			if err := validateManifestPath(logical); err != nil {
				return nil, fmt.Errorf("manifest dependency owner %q: %w", logical, err)
			}
			if _, ok := src.Entries[logical]; !ok {
				return nil, fmt.Errorf("manifest dependency owner %q is not in entries", logical)
			}
			copied := make([]string, len(dependencies))
			for i, dependency := range dependencies {
				if err := validateManifestPath(dependency); err != nil {
					return nil, fmt.Errorf("manifest dependency %q for %q: %w", dependency, logical, err)
				}
				if _, ok := src.Entries[dependency]; !ok {
					return nil, fmt.Errorf("manifest dependency %q for %q is not in entries", dependency, logical)
				}
				copied[i] = dependency
			}
			dst.Dependencies[logical] = copied
		}
	}
	return dst, nil
}

func validateManifestPath(p string) error {
	if p == "" {
		return fmt.Errorf("must not be empty")
	}
	if strings.ContainsRune(p, '\\') {
		return fmt.Errorf("must use forward slashes")
	}
	if err := validateMount(p); err != nil {
		return err
	}
	return nil
}

// LoadManifest reads publicDir/manifest.json.
func LoadManifest(publicDir string) (*Manifest, error) {
	path := filepath.Join(publicDir, ManifestFilename)
	f, err := os.Open(path) // #nosec G304 -- reads an app-selected asset path
	if err != nil {
		return nil, fmt.Errorf("assetmapper.LoadManifest: open %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck // read-only file close cannot affect the completed decode
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
	var out strings.Builder
	if err := m.Write(&out); err != nil {
		return err
	}
	if err := atomicWriteFile(path, []byte(out.String()), 0o644); err != nil {
		return fmt.Errorf("assetmapper.Manifest.Save: write %s: %w", path, err)
	}
	return nil
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
