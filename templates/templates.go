// Package templates loads sectioned HTML page templates and renders them
// through an http.ResponseWriter.
//
// Pages live in section directories. [_default] supplies fallback layouts
// and partials for other sections.
//
// Page keys are bare basenames for _default pages and "section/name" for
// every other section.
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
	pages map[string]*htmltpl.Template
}

// Source is one template tree contributing sections to a [Set].
type Source struct {
	FS  fs.FS
	Dir string
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
	return LoadSources([]Source{{FS: fsys, Dir: dir}}, funcMaps...)
}

// LoadSources builds one [Set] from several trees. A section may appear in
// only one source, and [DefaultSection] fallback works across sources.
func LoadSources(sources []Source, funcMaps ...FuncMap) (*Set, error) {
	merged := DefaultFuncs()
	for _, fm := range funcMaps {
		for k, v := range fm {
			merged[k] = v
		}
	}
	if err := checkFuncs(merged); err != nil {
		return nil, err
	}

	sections := map[string]*section{}
	origin := map[string]int{}
	for i, src := range sources {
		if src.FS == nil {
			return nil, fmt.Errorf("templates.LoadSources: source %d (%s) has a nil filesystem", i, src.Dir)
		}
		secs, err := readSections(src.FS, src.Dir)
		if err != nil {
			return nil, fmt.Errorf("templates.LoadSources: %w", err)
		}
		for name, sec := range secs {
			if first, dup := origin[name]; dup {
				return nil, fmt.Errorf("templates.LoadSources: section %q comes from source %d (%s) and source %d (%s)",
					name, first, sources[first].Dir, i, src.Dir)
			}
			origin[name] = i
			sections[name] = sec
		}
	}

	defSection, hasDefault := sections[DefaultSection]
	out := &Set{pages: map[string]*htmltpl.Template{}}

	// Keep diagnostics stable when several sections can fail.
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
			// Parse fallback partials first; section-local definitions win.
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
			return nil, fmt.Errorf("templates.Load: section %q has %d page(s) but no %s (and no %s/%s fallback)",
				name, len(sec.pages), LayoutFile, DefaultSection, LayoutFile)
		}

		for _, page := range sec.pages {
			files := append([]fileRef{layout}, partials...)
			files = append(files, page)
			t := htmltpl.New(LayoutFile).Funcs(merged)
			var err error
			for _, f := range files {
				if t, err = t.ParseFS(f.fsys, f.path); err != nil {
					return nil, fmt.Errorf("templates.Load: parse %s: %w", page.path, err)
				}
			}
			key := pageKey(name, page.path)
			if _, exists := out.pages[key]; exists {
				return nil, fmt.Errorf("templates.Load: duplicate page key %q (last from %s)", key, page.path)
			}
			out.pages[key] = t
		}
	}

	if len(out.pages) == 0 {
		return nil, fmt.Errorf("templates.Load: no pages found")
	}
	return out, nil
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
func checkFuncs(m FuncMap) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("templates.Load: invalid FuncMap: %v", p)
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

type fileRef struct {
	fsys fs.FS
	path string
}

type section struct {
	layout   fileRef
	partials []fileRef
	pages    []fileRef
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
