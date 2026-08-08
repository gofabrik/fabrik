package directive

import (
	"fmt"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
	"github.com/gofabrik/fabrik/web"
)

// Templates implements //fabrik:web:templates.
type Templates struct {
	decls       []*tplNode
	registered  bool
	contributed []contribution
	treeFS      func(dir string) fs.FS
}

// contribution is a FuncMap expression for the generated Load call.
type contribution struct {
	names []string
	build func(g *gen.Gen) (string, diag.Diagnostics)
}

func NewTemplates() *Templates {
	return &Templates{treeFS: os.DirFS}
}

// SetTreeFS lets validation read non-Go files through the engine overlay.
func (t *Templates) SetTreeFS(f func(dir string) fs.FS) { t.treeFS = f }

// ContributeFuncs registers a named FuncMap expression for Load.
func (t *Templates) ContributeFuncs(names []string, build func(g *gen.Gen) (string, diag.Diagnostics)) {
	t.contributed = append(t.contributed, contribution{names: names, build: build})
}

func (*Templates) Name() string { return "web:templates" }

func (*Templates) Meta() gen.Meta {
	return gen.Meta{
		Synopsis: "Template set from an embedded tree: [dir=templates]",
		Doc: "**`//fabrik:web:templates [dir=templates]`**\n\n" +
			"Declared on an exported `embed.FS` variable: the tree loads at " +
			"startup into a `*web.Templates`, injectable into handler structs " +
			"and providers. Templates live in sections; `_default` provides " +
			"fallback layouts and partials. `dir=` names the subdirectory " +
			"inside the FS. `*.html` files use html/template; non-HTML files " +
			"are ignored. Use `all:<dir>` so " +
			"layouts and `_`-prefixed partials are embedded. Several " +
			"packages may declare trees: shared can own `_default` while " +
			"each domain package ships its own section directories. A " +
			"section provided twice is an error, and every tree is " +
			"validated at generation time by loading it.\n\n" +
			"```go\n//fabrik:web:templates\n//go:embed all:templates\nvar Templates embed.FS\n```",
		Example: "//fabrik:web:templates",
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

// checkEmbedPattern requires all:<dir>.
func checkEmbedPattern(a gen.Annotation, dir string, ds *diag.Diagnostics) {
	found, covered := gen.EmbedCovers(a, "all:"+dir)
	if covered {
		return
	}
	if !found {
		ds.Error(a.Pos, "//fabrik:web:templates variable has no //go:embed",
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
		ds.Error(nd.pos, "//fabrik:web:templates must be on a package-level variable", "")
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

// Emit lazily binds one *web.Templates for all declared trees.
func (t *Templates) Emit(n any, g *gen.Gen) diag.Diagnostics {
	nd := n.(*tplNode)
	if nd.varName == "" || t.registered {
		return nil
	}
	t.registered = true
	g.BindLazyPathAt(templatesPath, nd.pos, func() (string, diag.Diagnostics) {
		decls := t.sortedDecls()
		for _, d := range decls {
			if !g.InValidationScope() {
				d.built = true
			}
		}
		webPkg := g.Import(webPath)
		first := decls[0]

		var args []string
		fn := webPkg + ".LoadTemplateSources"
		if len(decls) == 1 {
			fn = webPkg + ".LoadTemplates"
			args = []string{g.ImportPkg(first.pkg) + "." + first.varName, fmt.Sprintf("%q", first.dir)}
		} else {
			var b strings.Builder
			b.WriteString("[]" + webPkg + ".TemplateSource{\n")
			for _, d := range decls {
				fmt.Fprintf(&b, "{FS: %s.%s, Dir: %q},\n", g.ImportPkg(d.pkg), d.varName, d.dir)
			}
			b.WriteString("}")
			args = []string{b.String()}
		}
		// Later FuncMaps win; app helpers override contributed ones.
		// Contributed maps are stdlib html/template FuncMaps; web.FuncMap is
		// a defined type, so the call site converts.
		var ds diag.Diagnostics
		for _, c := range t.contributed {
			expr, cds := c.build(g)
			ds = append(ds, cds...)
			if expr != "" && !cds.HasFatal() {
				args = append(args, webPkg+".FuncMap("+expr+")")
			}
		}
		if fmType, found := g.LookupType(webPath, "FuncMap"); found && g.HasBinding(fmType, "") {
			// An app provider returning web.FuncMap supplies the static
			// helpers; appended last so they override contributed maps.
			expr, ids, ok := g.Instance(fmType, "")
			ds = append(ds, ids...)
			if ok && len(ids) == 0 {
				args = append(args, expr)
			}
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
		return v, ds
	})
	return nil
}

// unknownFuncName extracts the name from a parse error reporting an
// undefined template function.
func unknownFuncName(err error) (string, bool) {
	m := unknownFuncRE.FindStringSubmatch(err.Error())
	if m == nil {
		return "", false
	}
	return m[1], true
}

var unknownFuncRE = regexp.MustCompile(`function "([^"]+)" not defined`)

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

// Validate loads templates during wiring, before generated startup code runs.
func (t *Templates) Validate(*gen.Gen) diag.Diagnostics {
	var ds diag.Diagnostics
	if len(t.decls) == 0 {
		return ds
	}
	decls := t.sortedDecls()

	// Keep section-collision diagnostics on the duplicated declaration.
	owner := map[string]*tplNode{}
	collided := false
	for _, d := range decls {
		names, err := web.TemplateSections(t.treeFS(d.srcDir), d.dir)
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
		stubs := web.FuncMap{}
		for _, c := range t.contributed {
			for _, name := range c.names {
				stubs[name] = func(...any) any { return nil }
			}
		}
		sources := make([]web.TemplateSource, len(decls))
		for i, d := range decls {
			sources[i] = web.TemplateSource{FS: t.treeFS(d.srcDir), Dir: d.dir}
		}
		// Provider-supplied funcs are opaque here, so names the parse does
		// not know are stubbed one by one and reported; startup template
		// load performs the hard existence check.
		var assumed []string
		for {
			_, err := web.LoadTemplateSources(sources, stubs)
			if err == nil {
				break
			}
			name, ok := unknownFuncName(err)
			if !ok || stubs[name] != nil {
				ds.Error(decls[0].pos, err.Error(),
					"the templates are loaded and parsed at generation time; fix the tree and rerun")
				break
			}
			stubs[name] = func(...any) any { return nil }
			assumed = append(assumed, name)
		}
		if len(assumed) > 0 {
			sort.Strings(assumed)
			ds.Warn(decls[0].pos,
				fmt.Sprintf("template functions not registered at generation time: %s", strings.Join(assumed, ", ")),
				"assumed to come from a web.FuncMap or web.RequestFuncs provider; startup template load fails if one is missing")
		}
	}

	for _, d := range decls {
		if !d.built {
			ds.Warn(d.pos, fmt.Sprintf("templates %s are never used", d.varName),
				"inject *web.Templates into a handler struct or provider, or remove the directive")
		}
	}
	return ds
}

// MissingHint explains that template sets are injected by pointer.
func (t *Templates) MissingHint(ty types.Type) (string, bool) {
	if types.TypeString(types.Unalias(ty), nil) != webPath+".Templates" {
		return "", false
	}
	return "template sets are injected as pointers; take *web.Templates", true
}
