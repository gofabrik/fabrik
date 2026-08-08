package web

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"

	htmltpl "github.com/gofabrik/t/html/template"
)

// FuncMap maps template names to functions.
type FuncMap map[string]any

// defaultSection is the conventional section name whose partials act as the
// fallback for every other section.
const defaultSection = "_default"

// defaultEntry is the template an empty define renders: the conventional
// layout partial's own file name.
const defaultEntry = "_layout.html"

// RegionPrefix marks a template as a public region: a piece of a page that
// something outside the page may ask for by name.
//
// A page's group holds three kinds of template. Most are internal - the
// document, its structural blocks, and the file-named templates ParseFS
// creates - and naming one of those from outside is a mistake, not a feature.
// A define whose name begins with this prefix is the exception: it says the
// author intends that piece to be addressable, and its public name is
// whatever follows.
//
//	{{define "region/results"}}...{{end}}   // public name: results
//
// This is a statement about the template, not about any protocol. What asks
// for a region, and how it names one, is the caller's business.
const RegionPrefix = "region/"

func parseHTML(name string, funcs FuncMap, files []fileRef) (*htmltpl.Template, error) {
	// missingkey=error: a key a map payload does not carry is a mistake in the
	// page, not an empty string to render. A struct payload reports the same
	// mistake on its own, so this only matters while a caller still passes maps.
	t := htmltpl.New(name).Option("missingkey=error").Funcs(htmltpl.FuncMap(funcs))
	var err error
	for _, f := range files {
		if t, err = t.ParseFS(f.fsys, f.path); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// Templates is a parsed collection of sectioned HTML templates, and the
// renderer [NewAdapter] is usually configured with. It is safe for concurrent
// use.
//
// Templates live in section directories nested to any depth. Underscore-prefixed
// files are partials shared only with pages in the same directory; "_default"
// partials are shared with every section. Non-HTML files are ignored.
//
// Page names are extensionless section paths; "_default" pages use bare names.
//
// There is no layout rule. A page and its partials are parsed into one
// group, and [Templates.Render] executes whichever template in that group is
// named - the outer document, or one fragment of it. The one convention is
// the default: an empty define renders "_layout.html", the conventional
// layout partial, wrapping whatever the page calls "content".
//
// A page may also declare which of its pieces are addressable from outside,
// by naming them with [RegionPrefix]. [Templates.Region] resolves such a
// name and nothing else, which is what lets a caller act on a name it did
// not choose.
//
// Templates are parsed with missingkey=error, so reading a key a map payload
// does not carry fails the render instead of writing nothing.
type Templates struct {
	templates map[string]*htmltpl.Template
}

// TemplateSource is one template tree contributing sections to a [Templates].
type TemplateSource struct {
	FS  fs.FS
	Dir string
}

// LoadTemplates parses the HTML templates in the section directories under
// dir.
//
// Every section inherits the partials of "_default"; a section partial
// shadows a default partial with the same filename.
//
// funcMaps are merged after the built-in helpers in call order. Later maps
// override earlier maps, and nil maps are ignored. Static helpers and
// [RequestFuncs] stubs cannot share a name.
func LoadTemplates(fsys fs.FS, dir string, funcMaps ...FuncMap) (*Templates, error) {
	return LoadTemplateSources([]TemplateSource{{FS: fsys, Dir: dir}}, funcMaps...)
}

// LoadTemplateSources builds one [Templates] from several trees. A section
// may appear in only one source, and "_default" fallback works across
// sources.
//
// Static helpers and [RequestFuncs] stubs cannot share a name.
func LoadTemplateSources(sources []TemplateSource, funcMaps ...FuncMap) (*Templates, error) {
	merged := defaultFuncs()
	for _, fm := range funcMaps {
		for name, fn := range fm {
			if prev, ok := merged[name]; ok {
				_, prevStub := prev.(requestStub)
				_, nextStub := fn.(requestStub)
				// Request-func overlays must not shadow static helpers.
				if prevStub != nextStub {
					return nil, fmt.Errorf("web: template func %q is both a static helper and a request func", name)
				}
			}
			merged[name] = fn
		}
	}
	if err := checkFuncs(merged); err != nil {
		return nil, err
	}

	sections := map[string]*section{}
	origin := map[string]int{}
	for i, src := range sources {
		if src.FS == nil {
			return nil, fmt.Errorf("web: template source %d (%s) has a nil filesystem", i, src.Dir)
		}
		secs, err := readSections(src.FS, src.Dir)
		if err != nil {
			return nil, fmt.Errorf("web: load templates: %w", err)
		}
		for name, sec := range secs {
			if first, dup := origin[name]; dup {
				return nil, fmt.Errorf("web: template section %q comes from source %d (%s) and source %d (%s)",
					name, first, sources[first].Dir, i, src.Dir)
			}
			origin[name] = i
			sections[name] = sec
		}
	}

	orphans := make([]string, 0)
	for name, sec := range sections {
		if name != defaultSection && len(sec.pages) == 0 {
			orphans = append(orphans, name)
		}
	}
	if len(orphans) > 0 {
		sort.Strings(orphans)
		sec := sections[orphans[0]]
		bases := make([]string, 0, len(sec.partials))
		for _, p := range sec.partials {
			bases = append(bases, path.Base(p.path))
		}
		return nil, fmt.Errorf("web: section %q holds only partials (%s); without pages nothing can render them",
			orphans[0], strings.Join(bases, ", "))
	}

	defSection, hasDefault := sections[defaultSection]
	out := &Templates{templates: map[string]*htmltpl.Template{}}

	// Keep diagnostics stable when several sections can fail.
	names := make([]string, 0, len(sections))
	for name := range sections {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		sec := sections[name]

		var partials []fileRef
		if name != defaultSection && hasDefault {
			// Parse inherited partials first; section-local definitions win.
			local := map[string]bool{}
			for _, p := range sec.partials {
				local[path.Base(p.path)] = true
			}
			for _, p := range defSection.partials {
				if !local[path.Base(p.path)] {
					partials = append(partials, p)
				}
			}
		}
		partials = append(partials, sec.partials...)

		for _, page := range sec.pages {
			key := templateKey(name, page.path)
			files := append(append([]fileRef{}, partials...), page)
			t, err := parseHTML(key, merged, files)
			if err != nil {
				return nil, fmt.Errorf("web: parse %s: %w", page.path, err)
			}
			if err := checkRegions(key, t); err != nil {
				return nil, err
			}
			out.templates[key] = t
		}
	}

	if len(out.templates) == 0 {
		return nil, fmt.Errorf("web: no templates found")
	}
	return out, nil
}

// Render executes the template named define, from the group belonging to
// page, into w.
//
// define is any template that group can see: one the page declares, one it
// inherits from a partial, or a partial's own file name. A page rendered
// whole and one fragment of it swapped in later are the same call with a
// different name, which is what makes a fragment reachable at all. An empty
// define renders "_layout.html", the conventional layout partial.
//
// Lookup and execution errors leave w untouched; writer errors may leave
// partial output. Render does not set HTTP headers.
func (s *Templates) Render(w io.Writer, page, define string, data any) error {
	return s.RenderFuncs(w, page, define, data, nil)
}

// RenderFuncs renders with funcs overlaid for this execution; every name must
// be registered during loading, and nil funcs are equivalent to [Templates.Render].
func (s *Templates) RenderFuncs(w io.Writer, page, define string, data any, funcs FuncMap) error {
	if define == "" {
		define = defaultEntry
	}
	t, ok := s.templates[page]
	if !ok {
		return fmt.Errorf("web: unknown page %q", page)
	}
	entry := t.Lookup(define)
	if entry == nil {
		return fmt.Errorf("web: page %q defines no template %q", page, define)
	}

	buf := renderBuffers.Get().(*bytes.Buffer)
	defer func() {
		if buf.Cap() <= maxPooledBuffer {
			buf.Reset()
			renderBuffers.Put(buf)
		}
	}()

	if err := entry.ExecuteFuncs(buf, data, htmltpl.FuncMap(funcs)); err != nil {
		return fmt.Errorf("web: render %q %q: %w", page, define, err)
	}
	_, err := buf.WriteTo(w)
	return err
}

// checkFuncs turns html/template's invalid-FuncMap panic into a load error.
func checkFuncs(m FuncMap) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("web: invalid template FuncMap: %v", p)
		}
	}()
	htmltpl.New("check").Funcs(htmltpl.FuncMap(m))
	return nil
}

// TemplateSections returns the template section names a tree provides.
func TemplateSections(fsys fs.FS, dir string) ([]string, error) {
	if fsys == nil {
		return nil, fmt.Errorf("web: template sections: nil filesystem")
	}
	secs, err := readSections(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("web: template sections: %w", err)
	}
	out := make([]string, 0, len(secs))
	for name := range secs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// Region is one addressable piece of a page: the name callers use and the
// template that renders it.
type Region struct {
	// Name is the public name, without [RegionPrefix].
	Name string
	// Define is the template to render, which is what [Templates.Render]
	// takes.
	Define string
}

// Region resolves a page's public region name to the template that renders
// it, reporting whether the page declares one.
//
// The name is matched exactly against the page's declared regions, so a name
// from an untrusted source selects among what the page offers or nothing at
// all. It never becomes a template name of its own.
func (s *Templates) Region(page, name string) (define string, ok bool) {
	t, found := s.templates[page]
	if !found || name == "" {
		return "", false
	}
	define = RegionPrefix + name
	if d := t.Lookup(define); d == nil || d.Tree == nil {
		return "", false
	}
	return define, true
}

// Regions returns the regions a page declares, sorted by name. It reports nil
// for an unknown page.
func (s *Templates) Regions(page string) []Region {
	t, ok := s.templates[page]
	if !ok {
		return nil
	}
	var out []Region
	for _, d := range t.Templates() {
		if d.Tree == nil || !strings.HasPrefix(d.Name(), RegionPrefix) {
			continue
		}
		out = append(out, Region{Name: strings.TrimPrefix(d.Name(), RegionPrefix), Define: d.Name()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// checkRegions rejects a region with no name at load, where it is a typo to
// fix, rather than at the first request that cannot resolve it.
func checkRegions(page string, t *htmltpl.Template) error {
	for _, d := range t.Templates() {
		if d.Tree != nil && d.Name() == RegionPrefix {
			return fmt.Errorf("web: page %q declares a region with no name (%q)", page, RegionPrefix)
		}
	}
	return nil
}

type fileRef struct {
	fsys fs.FS
	path string
}

type section struct {
	partials []fileRef
	pages    []fileRef
}

// readSections groups HTML files by root-relative directory, deferring page-less
// section validation until sources are merged.
func readSections(fsys fs.FS, dir string) (map[string]*section, error) {
	out := map[string]*section{}
	err := fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		if d.IsDir() || path.Ext(d.Name()) != ".html" {
			return nil
		}
		rel := strings.TrimPrefix(p, dir+"/")
		secName := path.Dir(rel)
		if secName == "." {
			return fmt.Errorf("%s sits at the templates root; templates live in section directories", d.Name())
		}
		sec := out[secName]
		if sec == nil {
			sec = &section{}
			out[secName] = sec
		}
		ref := fileRef{fsys: fsys, path: p}
		if strings.HasPrefix(d.Name(), "_") {
			sec.partials = append(sec.partials, ref)
		} else {
			sec.pages = append(sec.pages, ref)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, sec := range out {
		sort.Slice(sec.partials, func(i, j int) bool { return sec.partials[i].path < sec.partials[j].path })
		sort.Slice(sec.pages, func(i, j int) bool { return sec.pages[i].path < sec.pages[j].path })
	}
	return out, nil
}

func templateKey(sectionName, filePath string) string {
	base := strings.TrimSuffix(path.Base(filePath), ".html")
	if sectionName == defaultSection {
		return base
	}
	return sectionName + "/" + base
}
