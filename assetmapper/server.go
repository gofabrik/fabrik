package assetmapper

import (
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
)

// Server serves an application's assets in either source or compiled mode.
type Server interface {
	Asset(logical string) (string, error)
	Handler() http.Handler
	FuncMap() template.FuncMap
	// ImportmapCSPSources returns the script sources the inline
	// importmap needs: its stable hash when compiled, 'unsafe-inline'
	// when serving live sources (no stable hash exists).
	ImportmapCSPSources() []string
}

// Kind names an asset serving mode.
type Kind string

const (
	KindSource   Kind = "source"
	KindCompiled Kind = "compiled"
)

// cspUnsafeInline is the CSP keyword source live source serving needs
// for the inline importmap script.
const cspUnsafeInline = "'unsafe-inline'"

// Options enables generated source/compiled switching when used as
// the assets configuration.
type Options struct {
	Kind string `yaml:"kind" default:"compiled"`
}

// Mode validates Kind, treating an empty value as KindCompiled.
func (c Options) Mode() (Kind, error) {
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
func NewSource(roots []Root, im *Importmap, opts ...BuildOption) (Server, error) {
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
	return &sourceServer{m: m, im: snapshot}, nil
}

type sourceServer struct {
	m  *Mapper
	im *Importmap
}

func (s *sourceServer) Asset(logical string) (string, error) { return s.m.Asset(logical) }
func (s *sourceServer) Handler() http.Handler                { return s.m.Handler() }
func (s *sourceServer) FuncMap() template.FuncMap            { return FuncMap(s.m, s.im) }

func (s *sourceServer) ImportmapCSPSources() []string { return []string{cspUnsafeInline} }
