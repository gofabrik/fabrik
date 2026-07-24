package assetmapper

import (
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
)

// Runtime exposes asset serving independently of source or compiled mode.
type Runtime interface {
	Asset(logical string) (string, error)
	Handler() http.Handler
	FuncMap() template.FuncMap
	// ImportmapCSPSource returns a stable CSP source hash, or false in source mode.
	ImportmapCSPSource() (source string, ok bool)
}

// Kind names an asset serving mode.
type Kind string

const (
	KindSource   Kind = "source"
	KindCompiled Kind = "compiled"
)

// RuntimeConfig enables generated source/compiled switching when used as the assets configuration.
type RuntimeConfig struct {
	Kind string `yaml:"kind" default:"compiled"`
}

// Mode validates Kind, treating an empty value as KindCompiled.
func (c RuntimeConfig) Mode() (Kind, error) {
	switch c.Kind {
	case "", string(KindCompiled):
		return KindCompiled, nil
	case string(KindSource):
		return KindSource, nil
	default:
		return "", fmt.Errorf("assets: invalid kind %q (want %q or %q)", c.Kind, KindSource, KindCompiled)
	}
}

// NewSource serves freshly read assets, resolves relative disk roots from the
// process working directory, and snapshots the importmap at construction.
// A nil im discovers a top-level importmap.json from the roots.
func NewSource(roots []Root, im *Importmap, opts ...BuildOption) (Runtime, error) {
	var cfg buildConfig
	for _, o := range opts {
		o(&cfg)
	}
	normalized, err := normalizeRoots("assetmapper.NewSource", roots)
	if err != nil {
		return nil, err
	}
	for i, r := range normalized {
		if _, err := fs.ReadDir(r.FS, "."); err != nil {
			return nil, fmt.Errorf("assetmapper.NewSource: Roots[%d]: %w; source roots resolve against the process working directory - run from the module root", i, err)
		}
	}
	if im == nil {
		im, err = discoverImportmap("assetmapper.NewSource", normalized)
		if err != nil {
			return nil, err
		}
	}
	m, err := New(Config{Roots: normalized, URLPrefix: cfg.urlPrefix})
	if err != nil {
		return nil, err
	}
	// Snapshot the importmap so rendering is stable after construction.
	snapshot := NewImportmap()
	for k, v := range im.Entries {
		snapshot.Entries[k] = v
	}
	return &sourceRuntime{m: m, im: snapshot}, nil
}

type sourceRuntime struct {
	m  *Mapper
	im *Importmap
}

func (s *sourceRuntime) Asset(logical string) (string, error) { return s.m.Asset(logical) }
func (s *sourceRuntime) Handler() http.Handler                { return s.m.Handler() }
func (s *sourceRuntime) FuncMap() template.FuncMap            { return FuncMap(s.m, s.im) }

func (s *sourceRuntime) ImportmapCSPSource() (string, bool) { return "", false }
