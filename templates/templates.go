// Package templates loads sectioned HTML templates and renders them into an
// io.Writer without setting HTTP headers.
//
// Templates live in section directories. [_default] supplies fallback
// layouts and partials for other sections. Non-HTML files are ignored.
//
// Names are bare basenames in [DefaultSection] and section-qualified
// elsewhere, without the extension.
package templates

import (
	"bytes"
	"fmt"
	htmltpl "html/template"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
)

// FuncMap is an alias for [html/template.FuncMap].
type FuncMap = htmltpl.FuncMap

// DefaultSection is the conventional section name whose layout
// and partials act as the fallback for every other section.
const DefaultSection = "_default"

// LayoutFile is the conventional layout filename.
const LayoutFile = "_layout.html"

func parseHTML(funcs FuncMap, files []fileRef) (*htmltpl.Template, error) {
	t := htmltpl.New(LayoutFile).Funcs(funcs)
	var err error
	for _, f := range files {
		if t, err = t.ParseFS(f.fsys, f.path); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// Set is a parsed collection of templates. It is safe for concurrent use.
type Set struct {
	templates map[string]*htmltpl.Template
}

// Source is one template tree contributing sections to a [Set].
type Source struct {
	FS  fs.FS
	Dir string
}

// Load parses the HTML templates in the section directories under dir.
//
// Every section resolves its layout and partials through [DefaultSection].
// A section with templates must resolve a [LayoutFile].
//
// funcMaps are merged after [DefaultFuncs] in call order. Later maps override
// earlier maps, and nil maps are ignored.
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
	out := &Set{templates: map[string]*htmltpl.Template{}}

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
			if len(sec.templates) == 0 {
				continue
			}
			return nil, fmt.Errorf("templates.Load: section %q has %d template(s) but no %s (and no %s/%s fallback)",
				name, len(sec.templates), LayoutFile, DefaultSection, LayoutFile)
		}

		for _, tp := range sec.templates {
			files := append([]fileRef{layout}, partials...)
			files = append(files, tp)
			t, err := parseHTML(merged, files)
			if err != nil {
				return nil, fmt.Errorf("templates.Load: parse %s: %w", tp.path, err)
			}
			out.templates[templateKey(name, tp.path)] = t
		}
	}

	if len(out.templates) == 0 {
		return nil, fmt.Errorf("templates.Load: no templates found")
	}
	return out, nil
}

// maxPooledBuffer keeps unusually large render buffers out of the pool.
const maxPooledBuffer = 64 << 10

var bufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// Render executes a named template into w.
//
// Lookup and execution errors leave w untouched; writer errors may leave
// partial output. Render does not set HTTP headers.
func (s *Set) Render(w io.Writer, template string, data any) error {
	t, ok := s.templates[template]
	if !ok {
		return fmt.Errorf("templates.Render: unknown template %q", template)
	}
	buf := bufPool.Get().(*bytes.Buffer)
	defer func() {
		if buf.Cap() <= maxPooledBuffer {
			buf.Reset()
			bufPool.Put(buf)
		}
	}()
	if err := t.ExecuteTemplate(buf, LayoutFile, data); err != nil {
		return fmt.Errorf("templates.Render %q: %w", template, err)
	}
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

// Names returns the template names, sorted.
func (s *Set) Names() []string {
	out := make([]string, 0, len(s.templates))
	for k := range s.templates {
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
	layout    fileRef
	partials  []fileRef
	templates []fileRef
}

// readSections treats direct child directories containing HTML templates as sections.
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
		empty := true
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			name := f.Name()
			if path.Ext(name) != ".html" {
				continue
			}
			ref := fileRef{fsys: fsys, path: path.Join(sub, name)}
			switch {
			case name == LayoutFile:
				sec.layout = ref
			case strings.HasPrefix(name, "_"):
				sec.partials = append(sec.partials, ref)
			default:
				sec.templates = append(sec.templates, ref)
			}
			empty = false
		}
		if empty {
			// Directories without HTML templates are not sections.
			continue
		}
		sort.Slice(sec.partials, func(i, j int) bool { return sec.partials[i].path < sec.partials[j].path })
		sort.Slice(sec.templates, func(i, j int) bool { return sec.templates[i].path < sec.templates[j].path })
		out[secName] = sec
	}
	return out, nil
}

func templateKey(sectionName, filePath string) string {
	base := strings.TrimSuffix(path.Base(filePath), ".html")
	if sectionName == DefaultSection {
		return base
	}
	return sectionName + "/" + base
}
