// Package directive integrates templates with the fabrik code generator.
package directive

import (
	"fmt"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
	"github.com/gofabrik/fabrik/templates"
)

const templatePath = "github.com/gofabrik/fabrik/templates"

const setPath = "*" + templatePath + ".Set"

// Templates implements //fabrik:templates.
type Templates struct {
	decls      []*tplNode
	registered bool
	helpers    []*helperNode
	byName     map[string]*helperNode
	treeFS     func(dir string) fs.FS
}

func NewTemplates() *Templates {
	return &Templates{
		byName: map[string]*helperNode{},
		treeFS: func(dir string) fs.FS { return os.DirFS(dir) },
	}
}

// SetTreeFS lets validation read non-Go files through the engine overlay.
func (t *Templates) SetTreeFS(f func(dir string) fs.FS) { t.treeFS = f }

func (*Templates) Name() string { return "templates" }

func (*Templates) Meta() gen.Meta {
	return gen.Meta{
		Synopsis: "Template set from an embedded tree: [dir=templates]",
		Doc: "**`//fabrik:templates [dir=templates]`**\n\n" +
			"Declared on an exported `embed.FS` variable: the tree loads at " +
			"startup into a `*templates.Set`, injectable into handler structs " +
			"and providers. Pages live in sections (`_default` is the shared " +
			"fallback for layouts and partials); `dir=` names the " +
			"subdirectory inside the FS. Use `all:<dir>` so `_layout.html` " +
			"and `_`-prefixed partials are embedded. Several packages may " +
			"declare trees: shared can own `_default` while each domain " +
			"package ships its own section directories. A " +
			"section provided twice is an error, and every tree is " +
			"validated at generation time by loading it.\n\n" +
			"```go\n//fabrik:templates\n//go:embed all:templates\nvar Templates embed.FS\n```",
		Example: "//fabrik:templates",
		Attrs: []gen.AttrSpec{
			{Key: "dir", Kind: gen.KindFreeform},
		},
		Tier: gen.TierBind,
	}
}

type tplNode struct {
	pos token.Position
	dir string

	varName string
	pkg     *types.Package
	srcDir  string
	built   bool
}

func (t *Templates) Parse(a gen.Annotation) (any, diag.Diagnostics) {
	args, ds := gen.ParseArgs(a, t.Meta())
	nd := &tplNode{pos: a.Pos, dir: "templates"}
	if d, ok := args.Attr["dir"]; ok {
		nd.dir = d.Text
	}
	checkEmbedPattern(a, nd.dir, &ds)
	if ds.HasFatal() {
		return nil, ds
	}
	return nd, ds
}

// embedPatterns splits a go:embed argument list, including quoted patterns.
func embedPatterns(rest string) []string {
	var out []string
	for i := 0; i < len(rest); {
		switch rest[i] {
		case ' ', '\t':
			i++
		case '"', '`':
			quote := rest[i]
			end := strings.IndexByte(rest[i+1:], quote)
			if end < 0 {
				return out
			}
			raw := rest[i : i+end+2]
			if p, err := strconv.Unquote(raw); err == nil {
				out = append(out, p)
			}
			i += end + 2
		default:
			end := strings.IndexAny(rest[i:], " \t")
			if end < 0 {
				out = append(out, rest[i:])
				return out
			}
			out = append(out, rest[i:i+end])
			i += end
		}
	}
	return out
}

// checkEmbedPattern requires all:<dir>; plain embeds skip layouts and partials.
func checkEmbedPattern(a gen.Annotation, dir string, ds *diag.Diagnostics) {
	embedded := false
	for _, line := range a.Doc {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "//go:embed ")
		if !ok {
			continue
		}
		embedded = true
		for _, pattern := range embedPatterns(rest) {
			if pattern == "all:"+dir {
				return
			}
		}
	}
	if !embedded {
		ds.Error(a.Pos, "//fabrik:templates variable has no //go:embed",
			fmt.Sprintf("add //go:embed all:%s above the variable", dir))
		return
	}
	ds.Error(a.Pos, fmt.Sprintf("the go:embed pattern does not cover all:%s", dir),
		fmt.Sprintf("use //go:embed all:%s - plain patterns drop _-prefixed files, other directories load an empty tree", dir))
}

func (t *Templates) Check(n any, ty gen.Typed) diag.Diagnostics {
	nd := n.(*tplNode)
	var ds diag.Diagnostics

	v, ok := ty.Target.(*types.Var)
	if !ok {
		ds.Error(nd.pos, "//fabrik:templates must be on a package-level variable", "")
		return ds
	}
	if !v.Exported() {
		ds.Error(nd.pos, fmt.Sprintf("templates variable %s is unexported", v.Name()),
			"generated code lives in package main; export the variable")
		return ds
	}
	if types.TypeString(types.Unalias(v.Type()), nil) != "embed.FS" {
		ds.Error(nd.pos, fmt.Sprintf("templates variable %s is not an embed.FS", v.Name()),
			"example: //go:embed all:templates\nvar Templates embed.FS")
		return ds
	}
	nd.varName = v.Name()
	nd.pkg = v.Pkg()
	nd.srcDir = filepath.Dir(nd.pos.Filename)
	t.decls = append(t.decls, nd)
	return ds
}

// Emit lazily binds one *templates.Set for all declared trees.
func (t *Templates) Emit(n any, g *gen.Gen) diag.Diagnostics {
	nd := n.(*tplNode)
	if nd.varName == "" || t.registered {
		return nil
	}
	t.registered = true
	g.BindLazyPath(setPath, func() (string, diag.Diagnostics) {
		decls := t.sortedDecls()
		for _, d := range decls {
			d.built = true
		}
		tplPkg := g.Import(templatePath)
		first := decls[0]

		var args []string
		fn := tplPkg + ".LoadSources"
		if len(decls) == 1 {
			fn = tplPkg + ".Load"
			args = []string{g.ImportPkg(first.pkg) + "." + first.varName, fmt.Sprintf("%q", first.dir)}
		} else {
			var b strings.Builder
			b.WriteString("[]" + tplPkg + ".Source{\n")
			for _, d := range decls {
				fmt.Fprintf(&b, "{FS: %s.%s, Dir: %q},\n", g.ImportPkg(d.pkg), d.varName, d.dir)
			}
			b.WriteString("}")
			args = []string{b.String()}
		}
		if fm := t.funcMapExpr(g, tplPkg); fm != "" {
			args = append(args, fm)
		}

		v := g.Var(first.pkg.Name() + first.varName)
		if len(decls) > 1 {
			v = g.Var("appTemplates")
		}
		g.Node(&gen.Call{
			Base: gen.Base{Phase: gen.PhaseWire, Origin: gen.Origin{Pos: first.pos}},
			Var:  v,
			Fn:   fn,
			Args: args,
			Err:  gen.ErrReturn,
		})
		return v, nil
	})
	return nil
}

func (t *Templates) sortedDecls() []*tplNode {
	decls := append([]*tplNode(nil), t.decls...)
	sort.Slice(decls, func(i, j int) bool {
		a, b := decls[i].pos, decls[j].pos
		if a.Filename != b.Filename {
			return a.Filename < b.Filename
		}
		return a.Line < b.Line
	})
	return decls
}

func (t *Templates) funcMapExpr(g *gen.Gen, tplPkg string) string {
	if len(t.helpers) == 0 {
		return ""
	}
	sorted := append([]*helperNode(nil), t.helpers...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].name < sorted[j].name })
	var b strings.Builder
	b.WriteString(tplPkg + ".FuncMap{\n")
	for _, h := range sorted {
		fmt.Fprintf(&b, "%q: %s.%s,\n", h.name, g.ImportPkg(h.pkg), h.fn)
	}
	b.WriteString("}")
	return b.String()
}

// Validate loads templates during wiring, before generated startup code runs.
func (t *Templates) Validate(*gen.Gen) diag.Diagnostics {
	var ds diag.Diagnostics
	if len(t.decls) == 0 {
		for _, h := range t.helpers {
			ds.Error(h.pos, "//fabrik:templates:func without any //fabrik:templates declaration",
				"declare a template set the helper can be parsed into")
		}
		return ds
	}
	decls := t.sortedDecls()

	// Keep section-collision diagnostics on the duplicated declaration.
	owner := map[string]*tplNode{}
	collided := false
	for _, d := range decls {
		names, err := templates.Sections(t.treeFS(d.srcDir), d.dir)
		if err != nil {
			continue
		}
		for _, name := range names {
			if first, dup := owner[name]; dup {
				ds.Error(d.pos, fmt.Sprintf("template section %q is already provided by the tree at %s", name, first.pos),
					"each section directory belongs to exactly one tree")
				collided = true
				continue
			}
			owner[name] = d
		}
	}

	if !collided {
		stubs := templates.FuncMap{}
		for _, h := range t.helpers {
			stubs[h.name] = func(...any) any { return nil }
		}
		sources := make([]templates.Source, len(decls))
		for i, d := range decls {
			sources[i] = templates.Source{FS: t.treeFS(d.srcDir), Dir: d.dir}
		}
		if _, err := templates.LoadSources(sources, stubs); err != nil {
			ds.Error(decls[0].pos, err.Error(),
				"the templates are loaded and parsed at generation time; fix the tree and rerun")
		}
	}

	for _, d := range decls {
		if !d.built {
			ds.Warn(d.pos, fmt.Sprintf("templates %s are never used", d.varName),
				"inject *templates.Set into a handler struct or provider, or remove the directive")
		}
	}
	return ds
}

// MissingHint explains that template sets are injected by pointer.
func (t *Templates) MissingHint(ty types.Type) (string, bool) {
	if types.TypeString(types.Unalias(ty), nil) != templatePath+".Set" {
		return "", false
	}
	return "template sets are injected as pointers; take *templates.Set", true
}

// Funcs implements //fabrik:templates:func.
type Funcs struct {
	templates *Templates
}

func NewFuncs(t *Templates) *Funcs { return &Funcs{templates: t} }

func (*Funcs) Name() string { return "templates:func" }

func (*Funcs) Meta() gen.Meta {
	return gen.Meta{
		Synopsis: "Template function: [name=NAME]",
		Doc: "**`//fabrik:templates:func [name=NAME]`**\n\n" +
			"Adds a package-level function to the template set's FuncMap. " +
			"The template-visible name defaults to the function name with a " +
			"lowered first letter (`HumanizeAge` -> `humanizeAge`); `name=` " +
			"overrides. The signature must be legal for html/template: one " +
			"result, or two with the second an `error`.\n\n" +
			"```go\n//fabrik:templates:func\nfunc HumanizeAge(t time.Time) string { ... }\n```",
		Example: "//fabrik:templates:func",
		Attrs: []gen.AttrSpec{
			{Key: "name", Kind: gen.KindFreeform},
		},
	}
}

type helperNode struct {
	pos  token.Position
	name string

	fn  string
	pkg *types.Package
}

func (f *Funcs) Parse(a gen.Annotation) (any, diag.Diagnostics) {
	args, ds := gen.ParseArgs(a, f.Meta())
	nd := &helperNode{pos: a.Pos}
	if nm, ok := args.Attr["name"]; ok {
		nd.name = nm.Text
		if !isFuncName(nd.name) {
			ds.Error(a.ArgPos(nm.Col), fmt.Sprintf("invalid template function name %q", nd.name),
				"use a short identifier: name=humanize")
		}
	}
	if ds.HasFatal() {
		return nil, ds
	}
	return nd, ds
}

func (f *Funcs) Check(n any, ty gen.Typed) diag.Diagnostics {
	nd := n.(*helperNode)
	var ds diag.Diagnostics

	fn, ok := ty.Target.(*types.Func)
	if !ok {
		ds.Error(nd.pos, "//fabrik:templates:func must be on a function", "")
		return ds
	}
	sig := fn.Signature()
	if sig.Recv() != nil {
		ds.Error(nd.pos, fmt.Sprintf("//fabrik:templates:func must be on a package-level function (func %s is a method)", fn.Name()),
			"move the helper out of the method set")
		return ds
	}
	if !fn.Exported() {
		ds.Error(nd.pos, fmt.Sprintf("template function %s is unexported", fn.Name()),
			"generated code lives in package main; export the function")
		return ds
	}
	if sig.TypeParams().Len() > 0 {
		ds.Error(nd.pos, fmt.Sprintf("template function %s cannot be generic (generated code cannot infer type arguments)", fn.Name()),
			"declare a concrete function")
		return ds
	}
	if !legalFuncSignature(sig) {
		ds.Error(nd.pos, fmt.Sprintf("template function %s has an illegal signature", fn.Name()),
			"html/template functions return one value, or two with the second an error")
		return ds
	}
	if nd.name == "" {
		nd.name = lowerFirst(fn.Name())
	}
	if first, dup := f.templates.byName[nd.name]; dup {
		ds.Error(nd.pos, fmt.Sprintf("duplicate template function name %q", nd.name),
			fmt.Sprintf("first declared at %s", first.pos))
		return ds
	}

	nd.fn = fn.Name()
	nd.pkg = fn.Pkg()
	f.templates.byName[nd.name] = nd
	f.templates.helpers = append(f.templates.helpers, nd)
	return ds
}

func (*Funcs) Emit(any, *gen.Gen) diag.Diagnostics { return nil }

func legalFuncSignature(sig *types.Signature) bool {
	res := sig.Results()
	switch res.Len() {
	case 1:
		return true
	case 2:
		return types.TypeString(types.Unalias(res.At(1).Type()), nil) == "error"
	}
	return false
}

func isFuncName(s string) bool {
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b == '_' || (i > 0 && b >= '0' && b <= '9') {
			continue
		}
		return false
	}
	return s != ""
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	if b := s[0]; b >= 'A' && b <= 'Z' {
		return string(b+'a'-'A') + s[1:]
	}
	return s
}
