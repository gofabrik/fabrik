// Package templates loads sectioned HTML page templates and renders them
// through an http.ResponseWriter.
//
// Pages live in section directories. [_default] supplies fallback layouts
// and partials for other sections.
//
// Page keys are bare basenames for _default pages and "section/name" for
// every other section.
//
// [LoadSources] combines strict trees. [LoadLayers] combines ordered layers,
// where later layers override earlier ones file by file.
package templates

import (
	"bytes"
	"fmt"
	htmltpl "html/template"
	"io/fs"
	"net/http"
	"path"
	"sort"
	"strings"
)

// FuncMap is an alias for [html/template.FuncMap].
type FuncMap = htmltpl.FuncMap

// DefaultSection is the conventional section name whose layout
// and partials act as the fallback for every other section.
const DefaultSection = "_default"

// LayoutFile is the conventional filename whose presence marks a
// section's base layout.
const LayoutFile = "_layout.html"

// Set is a parsed collection of pages. It is safe for concurrent use.
type Set struct {
	pages     map[string]*htmltpl.Template
	overrides []Override
}

// Source is one template tree contributing sections to a [Set].
type Source struct {
	FS  fs.FS
	Dir string
}

// Kind names the sort of file an [Override] shadowed.
type Kind int

const (
	KindPage Kind = iota
	KindPartial
	KindLayout
)

func (k Kind) String() string {
	switch k {
	case KindPage:
		return "page"
	case KindPartial:
		return "partial"
	case KindLayout:
		return "layout"
	default:
		return "unknown"
	}
}

// Ref identifies a source within a layer.
type Ref struct {
	Layer  int
	Source int
	Dir    string
}

// Override records a cross-layer file shadow within one section.
type Override struct {
	Section string
	// Name is the file's base name, including the .html extension.
	Name   string
	Kind   Kind
	Winner Ref
	Loser  Ref
}

// Load walks dir under fsys and parses every page it finds.
//
// Each dir/<section>/ directory is a section. [LayoutFile] is the section
// layout, _*.html files are partials, and other *.html files are pages.
// Sections without their own layout or partials fall back to [DefaultSection].
//
// funcMaps are merged after [DefaultFuncs] in call order. Later maps override
// earlier maps, and nil maps are ignored.
//
// Load fails if a page has no resolvable layout or any file fails to parse.
func Load(fsys fs.FS, dir string, funcMaps ...FuncMap) (*Set, error) {
	return loadImpl("templates.Load", false, [][]Source{{{FS: fsys, Dir: dir}}}, funcMaps)
}

// LoadSources builds one [Set] from several trees. A section may appear in
// only one source, and [DefaultSection] fallback works across sources.
func LoadSources(sources []Source, funcMaps ...FuncMap) (*Set, error) {
	return loadImpl("templates.LoadSources", false, [][]Source{sources}, funcMaps)
}

// LoadLayers builds one [Set] from ordered layers of sources.
//
// Within a layer, sections must be disjoint as in [LoadSources]. Across
// layers, later files override earlier files by base name within a section.
//
// Empty layers are ignored. A non-empty layer holding a Source with a nil
// filesystem is an error. [Set.Overrides] reports cross-layer shadows.
func LoadLayers(layers [][]Source, funcMaps ...FuncMap) (*Set, error) {
	return loadImpl("templates.LoadLayers", len(layers) > 1, layers, funcMaps)
}

func loadImpl(caller string, multiLayer bool, layers [][]Source, funcMaps []FuncMap) (*Set, error) {
	merged := DefaultFuncs()
	for _, fm := range funcMaps {
		for k, v := range fm {
			merged[k] = v
		}
	}
	if err := checkFuncs(caller, merged); err != nil {
		return nil, err
	}

	layerSecs := make([]map[string]*section, len(layers))
	for l, srcs := range layers {
		ls, err := mergeLayer(caller, multiLayer, l, srcs)
		if err != nil {
			return nil, err
		}
		layerSecs[l] = ls
	}

	nameSet := map[string]bool{}
	for _, ls := range layerSecs {
		for name := range ls {
			nameSet[name] = true
		}
	}
	names := make([]string, 0, len(nameSet))
	for name := range nameSet {
		names = append(names, name)
	}
	sort.Strings(names)

	sections := map[string]*section{}
	var overrides []Override
	rec := func(o Override) { overrides = append(overrides, o) }
	for _, name := range names {
		var contribs []*section
		for l := range layerSecs {
			if sec, ok := layerSecs[l][name]; ok {
				contribs = append(contribs, sec)
			}
		}
		sections[name] = mergeSection(name, contribs, rec)
	}

	out, err := buildPages(caller, sections, merged)
	if err != nil {
		return nil, err
	}
	out.overrides = overrides
	return out, nil
}

// mergeLayer keeps section ownership strict inside one layer.
func mergeLayer(caller string, multiLayer bool, layerIdx int, srcs []Source) (map[string]*section, error) {
	secs := map[string]*section{}
	origin := map[string]Ref{}
	for si, src := range srcs {
		if src.FS == nil {
			return nil, fmt.Errorf("%s: %s has a nil filesystem", caller, locRef(multiLayer, Ref{Layer: layerIdx, Source: si, Dir: src.Dir}))
		}
		got, err := readSections(src.FS, src.Dir)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", caller, err)
		}
		for name, sec := range got {
			r := Ref{Layer: layerIdx, Source: si, Dir: src.Dir}
			if first, dup := origin[name]; dup {
				return nil, fmt.Errorf("%s: section %q comes from %s and %s",
					caller, name, locRef(multiLayer, first), locRef(multiLayer, r))
			}
			origin[name] = r
			stampOrigin(sec, r)
			secs[name] = sec
		}
	}
	return secs, nil
}

// mergeSection applies cross-layer overrides for one section.
func mergeSection(name string, contribs []*section, rec func(Override)) *section {
	out := &section{}
	haveLayout := false
	for _, c := range contribs {
		if c.layout.path != "" {
			if haveLayout {
				rec(Override{Section: name, Name: LayoutFile, Kind: KindLayout, Winner: c.layout.from, Loser: out.layout.from})
			}
			out.layout = c.layout
			haveLayout = true
		}
	}
	out.pages = mergeFiles(name, KindPage, contribs, func(s *section) []fileRef { return s.pages }, rec)
	out.partials = mergeFiles(name, KindPartial, contribs, func(s *section) []fileRef { return s.partials }, rec)
	return out
}

// mergeFiles preserves layer parse order, so later partial definitions win
// even when filenames differ.
func mergeFiles(name string, kind Kind, contribs []*section, pick func(*section) []fileRef, rec func(Override)) []fileRef {
	var order []fileRef
	idx := map[string]int{}
	for _, c := range contribs {
		for _, f := range pick(c) {
			b := path.Base(f.path)
			if prev, ok := idx[b]; ok {
				rec(Override{Section: name, Name: b, Kind: kind, Winner: f.from, Loser: order[prev].from})
				order[prev] = fileRef{}
			}
			order = append(order, f)
			idx[b] = len(order) - 1
		}
	}
	out := order[:0]
	for _, f := range order {
		if f.path != "" {
			out = append(out, f)
		}
	}
	return out
}

// buildPages applies [DefaultSection] fallback after layer merging.
func buildPages(caller string, sections map[string]*section, merged FuncMap) (*Set, error) {
	defSection, hasDefault := sections[DefaultSection]
	out := &Set{pages: map[string]*htmltpl.Template{}}

	names := make([]string, 0, len(sections))
	for name := range sections {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		sec := sections[name]
		layout := sec.layout
		var partials []fileRef
		if name != DefaultSection && hasDefault {
			if layout.path == "" {
				layout = defSection.layout
			}
			localNames := map[string]bool{}
			for _, p := range sec.partials {
				localNames[path.Base(p.path)] = true
			}
			for _, p := range defSection.partials {
				if !localNames[path.Base(p.path)] {
					partials = append(partials, p)
				}
			}
		}
		partials = append(partials, sec.partials...)

		if layout.path == "" {
			if len(sec.pages) == 0 {
				continue
			}
			return nil, fmt.Errorf("%s: section %q has %d page(s) but no %s (and no %s/%s fallback)",
				caller, name, len(sec.pages), LayoutFile, DefaultSection, LayoutFile)
		}

		for _, page := range sec.pages {
			files := append([]fileRef{layout}, partials...)
			files = append(files, page)
			t := htmltpl.New(LayoutFile).Funcs(merged)
			var err error
			for _, f := range files {
				if t, err = t.ParseFS(f.fsys, f.path); err != nil {
					return nil, fmt.Errorf("%s: parse %s: %w", caller, page.path, err)
				}
			}
			key := pageKey(name, page.path)
			if _, exists := out.pages[key]; exists {
				return nil, fmt.Errorf("%s: duplicate page key %q (last from %s)", caller, key, page.path)
			}
			out.pages[key] = t
		}
	}

	if len(out.pages) == 0 {
		return nil, fmt.Errorf("%s: no pages found", caller)
	}
	return out, nil
}

func locRef(multiLayer bool, r Ref) string {
	if multiLayer {
		return fmt.Sprintf("layer %d source %d (%s)", r.Layer, r.Source, r.Dir)
	}
	return fmt.Sprintf("source %d (%s)", r.Source, r.Dir)
}

// Render executes a page by key.
//
// Render buffers output; errors leave the response untouched.
// On success it sets the HTML Content-Type and flushes the buffer.
func (s *Set) Render(w http.ResponseWriter, name string, data any) error {
	t, ok := s.pages[name]
	if !ok {
		return fmt.Errorf("templates.Render: unknown page %q", name)
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, LayoutFile, data); err != nil {
		return fmt.Errorf("templates.Render %q: %w", name, err)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, err := buf.WriteTo(w)
	return err
}

// checkFuncs turns html/template's invalid-FuncMap panic into a load error.
func checkFuncs(caller string, m FuncMap) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("%s: invalid FuncMap: %v", caller, p)
		}
	}()
	htmltpl.New("check").Funcs(m)
	return nil
}

// Sections returns the template section names a tree provides.
func Sections(fsys fs.FS, dir string) ([]string, error) {
	if fsys == nil {
		return nil, fmt.Errorf("templates.Sections: nil filesystem")
	}
	secs, err := readSections(fsys, dir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(secs))
	for name := range secs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// Names returns the registered page keys, sorted.
func (s *Set) Names() []string {
	out := make([]string, 0, len(s.pages))
	for k := range s.pages {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Overrides returns the cross-layer shadows recorded during [LoadLayers].
// The order is stable and deterministic: by section name, and within a section
// the layout override, then page overrides, then partial overrides.
// It returns a fresh copy each call and is empty for [Load] and [LoadSources].
func (s *Set) Overrides() []Override {
	out := make([]Override, len(s.overrides))
	copy(out, s.overrides)
	return out
}

type fileRef struct {
	fsys fs.FS
	path string
	from Ref
}

type section struct {
	layout   fileRef
	partials []fileRef
	pages    []fileRef
}

func stampOrigin(sec *section, r Ref) {
	if sec.layout.path != "" {
		sec.layout.from = r
	}
	for i := range sec.partials {
		sec.partials[i].from = r
	}
	for i := range sec.pages {
		sec.pages[i].from = r
	}
}

// readSections treats direct child directories containing templates as sections.
func readSections(fsys fs.FS, dir string) (map[string]*section, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	out := map[string]*section{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		secName := e.Name()
		sub := path.Join(dir, secName)
		files, err := fs.ReadDir(fsys, sub)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", sub, err)
		}
		sec := &section{}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			name := f.Name()
			if !strings.HasSuffix(name, ".html") {
				continue
			}
			ref := fileRef{fsys: fsys, path: path.Join(sub, name)}
			switch {
			case name == LayoutFile:
				sec.layout = ref
			case strings.HasPrefix(name, "_"):
				sec.partials = append(sec.partials, ref)
			default:
				sec.pages = append(sec.pages, ref)
			}
		}
		if sec.layout.path == "" && len(sec.partials) == 0 && len(sec.pages) == 0 {
			// Asset or working directories are not template sections.
			continue
		}
		sort.Slice(sec.partials, func(i, j int) bool { return sec.partials[i].path < sec.partials[j].path })
		sort.Slice(sec.pages, func(i, j int) bool { return sec.pages[i].path < sec.pages[j].path })
		out[secName] = sec
	}
	return out, nil
}

func pageKey(sectionName, pagePath string) string {
	base := strings.TrimSuffix(path.Base(pagePath), ".html")
	if sectionName == DefaultSection {
		return base
	}
	return sectionName + "/" + base
}
