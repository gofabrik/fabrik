// Package templates loads sectioned HTML and text templates and renders
// them into an io.Writer without setting HTTP headers.
//
// Templates live in section directories. [_default] supplies fallback
// layouts and partials for other sections. HTML and text templates resolve
// their layouts and partials independently.
//
// Names are bare basenames in [DefaultSection] and section-qualified elsewhere;
// HTML names omit the extension, while text names retain it.
package templates

import (
	"bytes"
	"context"
	"fmt"
	htmltpl "html/template"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
	texttpl "text/template"
	"text/template/parse"
)

// FuncMap is an alias for [html/template.FuncMap].
type FuncMap = htmltpl.FuncMap

// DefaultSection is the conventional section name whose layout
// and partials act as the fallback for every other section.
const DefaultSection = "_default"

// LayoutFile is the conventional HTML layout filename.
const LayoutFile = "_layout.html"

// TextLayoutFile is the conventional text layout filename.
const TextLayoutFile = "_layout.txt"

// TextBody is the template name used by text layouts for the body.
const TextBody = "content"

type executor interface {
	ExecuteTemplate(w io.Writer, name string, data any) error
}

type plane struct {
	ext    string
	layout string
}

var planes = []plane{
	{".html", LayoutFile},
	{".txt", TextLayoutFile},
}

func planeFor(ext string) *plane {
	for i := range planes {
		if planes[i].ext == ext {
			return &planes[i]
		}
	}
	return nil
}

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
	templates map[string]tmpl
}

type tmpl struct {
	tpl      executor
	root     string
	bindings *bindings // nil unless the Set was loaded with Globals
}

// Source is one template tree contributing sections to a [Set].
type Source struct {
	FS  fs.FS
	Dir string
}

// Load parses HTML and text templates in the section directories under dir.
//
// Each format resolves layouts and partials independently through
// [DefaultSection]. HTML templates require a [LayoutFile]; text templates
// use a [TextLayoutFile] when available and otherwise execute directly.
//
// [Funcs] maps are merged after [DefaultFuncs] in call order, and [Globals]
// after those. Both formats use the same functions; values with
// html/template's trusted types render unescaped in text templates.
//
// Load rejects text bodies that declare named templates.
func Load(fsys fs.FS, dir string, opts ...Option) (*Set, error) {
	return LoadSources([]Source{{FS: fsys, Dir: dir}}, opts...)
}

// LoadSources builds one [Set] from several trees. A section may appear in
// only one source, and [DefaultSection] fallback works across sources.
func LoadSources(sources []Source, opts ...Option) (*Set, error) {
	var o options
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		opt(&o)
	}
	merged := DefaultFuncs()
	for _, fm := range o.funcs {
		for k, v := range fm {
			merged[k] = v
		}
	}
	// Global names must exist before parsing.
	var globalFuncs FuncMap
	if o.globals != nil {
		var err error
		if globalFuncs, err = discoverGlobals(o.globals); err != nil {
			return nil, err
		}
		for k, v := range globalFuncs {
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
	out := &Set{templates: map[string]tmpl{}}

	// Keep diagnostics stable when several sections can fail.
	names := make([]string, 0, len(sections))
	for name := range sections {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		sec := sections[name]
		for _, pl := range planes {
			sp := sec.planes[pl.ext]
			if sp == nil {
				sp = &sectionPlane{}
			}
			layout := sp.layout
			var partials []fileRef
			if name != DefaultSection && hasDefault {
				if dsp := defSection.planes[pl.ext]; dsp != nil {
					if layout.path == "" {
						layout = dsp.layout
					}
					// Parse fallback partials first; section-local definitions win.
					localNames := map[string]bool{}
					for _, p := range sp.partials {
						localNames[path.Base(p.path)] = true
					}
					for _, p := range dsp.partials {
						if !localNames[path.Base(p.path)] {
							partials = append(partials, p)
						}
					}
				}
			}
			partials = append(partials, sp.partials...)

			if layout.path == "" && pl.ext == ".html" {
				if len(sp.templates) == 0 {
					continue
				}
				return nil, fmt.Errorf("templates.Load: section %q has %d template(s) but no %s (and no %s/%s fallback)",
					name, len(sp.templates), LayoutFile, DefaultSection, LayoutFile)
			}

			for _, tp := range sp.templates {
				var t executor
				var clone func(FuncMap) (executor, error)
				var err error
				root := pl.layout
				if pl.ext == ".txt" {
					var tt *texttpl.Template
					tt, root, err = parseTextTemplate(merged, layout, partials, tp)
					if tt != nil {
						t, clone = tt, textCloner(tt)
					}
				} else {
					files := append([]fileRef{layout}, partials...)
					files = append(files, tp)
					var ht *htmltpl.Template
					ht, err = parseHTML(merged, files)
					if ht != nil {
						t, clone = ht, htmlCloner(ht)
					}
				}
				if err != nil {
					return nil, fmt.Errorf("templates.Load: parse %s: %w", tp.path, err)
				}
				key := templateKey(name, tp.path, pl.ext)
				if _, exists := out.templates[key]; exists {
					return nil, fmt.Errorf("templates.Load: duplicate template %q (last from %s)", key, tp.path)
				}
				entry := tmpl{tpl: t, root: root}
				if o.globals != nil {
					// Keep the parsed tree unexecuted so the pool can clone it.
					entry.tpl = nil
					entry.bindings = newBindings(o.globals, globalFuncs, clone)
				}
				out.templates[key] = entry
			}
		}
	}

	if len(out.templates) == 0 {
		return nil, fmt.Errorf("templates.Load: no templates found")
	}
	return out, nil
}

// parseTextTemplate rejects named definitions so layouts do not change body syntax.
func parseTextTemplate(funcs FuncMap, layout fileRef, partials []fileRef, tp fileRef) (*texttpl.Template, string, error) {
	raw, err := fs.ReadFile(tp.fsys, tp.path)
	if err != nil {
		return nil, "", err
	}
	// The actual parse checks functions after structural validation.
	probe := parse.New(TextBody)
	probe.Mode = parse.SkipFuncCheck
	probeTrees := map[string]*parse.Tree{}
	if _, err := probe.Parse(string(raw), "", "", probeTrees); err != nil {
		return nil, "", err
	}
	// A define-only body replaces the empty root, so count alone cannot detect it.
	if len(probeTrees) > 1 || probeTrees[TextBody] != probe {
		return nil, "", fmt.Errorf("text templates are raw bodies; move {{define}} blocks into _*.txt partials")
	}

	t := texttpl.New(TextLayoutFile).Funcs(texttpl.FuncMap(funcs))
	files := partials
	if layout.path != "" {
		files = append([]fileRef{layout}, partials...)
	}
	for _, f := range files {
		if t, err = t.ParseFS(f.fsys, f.path); err != nil {
			return nil, "", err
		}
	}
	if _, err := t.New(TextBody).Parse(string(raw)); err != nil {
		return nil, "", err
	}
	root := TextLayoutFile
	if layout.path == "" {
		root = TextBody
	}
	return t, root, nil
}

// Render executes a named template into w. Functions registered with
// [Globals] read ctx.
//
// Lookup and execution errors leave w untouched; writer errors may leave
// partial output. Render does not set HTTP headers.
func (s *Set) Render(ctx context.Context, w io.Writer, template string, data any) error {
	t, ok := s.templates[template]
	if !ok {
		return fmt.Errorf("templates.Render: unknown template %q", template)
	}
	exec := t.tpl
	if t.bindings != nil {
		b, err := t.bindings.get(ctx)
		if err != nil {
			return fmt.Errorf("templates.Render %q: %w", template, err)
		}
		defer t.bindings.put(b)
		exec = b.tpl
	}

	buf := buffers.Get().(*bytes.Buffer)
	defer putBuffer(buf)
	if err := exec.ExecuteTemplate(buf, t.root, data); err != nil {
		return fmt.Errorf("templates.Render %q: %w", template, err)
	}
	_, err := buf.WriteTo(w)
	return err
}

// buffers holds the scratch space renders build their output in.
var buffers = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// maxPooledBuffer keeps one outsized page from pinning its buffer for
// the life of the process.
const maxPooledBuffer = 64 << 10

func putBuffer(b *bytes.Buffer) {
	if b.Cap() > maxPooledBuffer {
		return
	}
	b.Reset()
	buffers.Put(b)
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
	planes map[string]*sectionPlane
}

type sectionPlane struct {
	layout    fileRef
	partials  []fileRef
	templates []fileRef
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
		sec := &section{planes: map[string]*sectionPlane{}}
		empty := true
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			name := f.Name()
			pl := planeFor(path.Ext(name))
			if pl == nil {
				continue
			}
			sp := sec.planes[pl.ext]
			if sp == nil {
				sp = &sectionPlane{}
				sec.planes[pl.ext] = sp
			}
			ref := fileRef{fsys: fsys, path: path.Join(sub, name)}
			switch {
			case name == pl.layout:
				sp.layout = ref
			case strings.HasPrefix(name, "_"):
				sp.partials = append(sp.partials, ref)
			default:
				sp.templates = append(sp.templates, ref)
			}
			empty = false
		}
		if empty {
			// Asset or working directories are not template sections.
			continue
		}
		for _, sp := range sec.planes {
			sort.Slice(sp.partials, func(i, j int) bool { return sp.partials[i].path < sp.partials[j].path })
			sort.Slice(sp.templates, func(i, j int) bool { return sp.templates[i].path < sp.templates[j].path })
		}
		out[secName] = sec
	}
	return out, nil
}

func templateKey(sectionName, filePath, ext string) string {
	base := path.Base(filePath)
	if ext == ".html" {
		base = strings.TrimSuffix(base, ext)
	}
	if sectionName == DefaultSection {
		return base
	}
	return sectionName + "/" + base
}
