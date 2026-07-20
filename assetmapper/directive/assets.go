// Package directive integrates assetmapper with the fabrik code generator.
package directive

import (
	"fmt"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gofabrik/fabrik/assetmapper"
	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
	routerdir "github.com/gofabrik/fabrik/router/directive"
)

const assetmapperPath = "github.com/gofabrik/fabrik/assetmapper"

const compiledPath = "*" + assetmapperPath + ".Compiled"

// urlPrefix is fabrik's asset URL root; standalone users pick their
// own via assetmapper.WithURLPrefix.
const urlPrefix = "/assets/"

// contributedFuncs names the template helpers Compiled.FuncMap carries.
var contributedFuncs = []string{
	"asset",
	"importmap",
	"importmap_nonce",
	"module_preload_links",
	"module_preload_links_nonce",
}

// FuncContributor receives the asset template helpers; the templates
// directive implements it. Nil disables template integration.
type FuncContributor interface {
	ContributeFuncs(names []string, build func(g *gen.Gen) (string, diag.Diagnostics))
}

// Assets implements //fabrik:assets.
type Assets struct {
	host  *routerdir.Host
	funcs FuncContributor

	decls      []*assetNode
	registered bool
	treeFS     func(dir string) fs.FS
}

// NewAssets returns an Assets directive for one run.
func NewAssets(host *routerdir.Host, funcs FuncContributor) *Assets {
	return &Assets{
		host:   host,
		funcs:  funcs,
		treeFS: os.DirFS,
	}
}

// SetTreeFS lets validation read non-Go files through the engine overlay.
func (as *Assets) SetTreeFS(f func(dir string) fs.FS) { as.treeFS = f }

func (*Assets) Name() string { return "assets" }

func (*Assets) Meta() gen.Meta {
	return gen.Meta{
		Synopsis: "Asset pipeline from an embedded tree: [dir=assets]",
		Doc: "**`//fabrik:assets [dir=assets]`**\n\n" +
			"Declared on an exported `embed.FS` variable: the sources compile " +
			"in memory at startup - content-hashed URLs, JS / CSS references " +
			"rewritten to hashed names, importmap rendering - and serve under " +
			"`/assets/`. Template sets gain the `asset` and `importmap` " +
			"helpers automatically. `dir=` names the subdirectory inside the " +
			"FS. Use `all:<dir>`: plain patterns silently drop `_`-prefixed " +
			"and dot-prefixed files. Several packages may declare trees; they " +
			"union into one namespace, and a path provided twice is an error. " +
			"An `importmap.json` at the top of one tree maps bare module " +
			"specifiers; edits to assets or the importmap never require a " +
			"rewire. Every tree is compile-checked at generation time.\n\n" +
			"```go\n//fabrik:assets\n//go:embed all:assets\nvar Assets embed.FS\n```",
		Example: "//fabrik:assets",
		Attrs: []gen.AttrSpec{
			{Key: "dir", Kind: gen.KindFreeform},
		},
		Tier: gen.TierBind,
	}
}

type assetNode struct {
	pos token.Position
	dir string

	varName string
	pkg     *types.Package
	srcDir  string
}

func (as *Assets) Parse(a gen.Annotation) (any, diag.Diagnostics) {
	args, ds := gen.ParseArgs(a, as.Meta())
	nd := &assetNode{pos: a.Pos, dir: "assets"}
	if d, ok := args.Attr["dir"]; ok {
		nd.dir = d.Text
	}
	checkEmbedPattern(a, nd.dir, &ds)
	if ds.HasFatal() {
		return nil, ds
	}
	return nd, ds
}

// checkEmbedPattern requires all:<dir>; a plain pattern would drop
// files whose absence surfaces as production 404s.
func checkEmbedPattern(a gen.Annotation, dir string, ds *diag.Diagnostics) {
	found, covered := gen.EmbedCovers(a, "all:"+dir)
	if covered {
		return
	}
	if !found {
		ds.Error(a.Pos, "//fabrik:assets variable has no //go:embed",
			fmt.Sprintf("add //go:embed all:%s above the variable", dir))
		return
	}
	ds.Error(a.Pos, fmt.Sprintf("the go:embed pattern does not cover all:%s", dir),
		fmt.Sprintf("use //go:embed all:%s - plain patterns silently drop _-prefixed files (like _variables.css) and dot-prefixed files (like .well-known/), and a missing asset is a runtime 404", dir))
}

func (as *Assets) Check(n any, ty gen.Typed) diag.Diagnostics {
	nd := n.(*assetNode)
	var ds diag.Diagnostics

	v, ok := ty.Target.(*types.Var)
	if !ok {
		ds.Error(nd.pos, "//fabrik:assets must be on a package-level variable", "")
		return ds
	}
	if !v.Exported() {
		ds.Error(nd.pos, fmt.Sprintf("assets variable %s is unexported", v.Name()),
			"generated code lives in package main; export the variable")
		return ds
	}
	if types.TypeString(types.Unalias(v.Type()), nil) != "embed.FS" {
		ds.Error(nd.pos, fmt.Sprintf("assets variable %s is not an embed.FS", v.Name()),
			"example: //go:embed all:assets\nvar Assets embed.FS")
		return ds
	}
	nd.varName = v.Name()
	nd.pkg = v.Pkg()
	nd.srcDir = filepath.Dir(nd.pos.Filename)
	as.decls = append(as.decls, nd)
	return ds
}

// PrepareNode binds the HTTP server for asset routes.
func (as *Assets) PrepareNode(_ any, g *gen.Gen) { as.host.BindHTTPServer(g) }

// Emit binds the compiled assets, registers the serving route, and
// contributes the template helpers, once for all declared trees.
func (as *Assets) Emit(n any, g *gen.Gen) diag.Diagnostics {
	nd := n.(*assetNode)
	if nd.varName == "" || as.registered {
		return nil
	}
	as.registered = true

	g.BindLazyPath(compiledPath, func() (string, diag.Diagnostics) {
		decls := as.sortedDecls()
		amPkg := g.Import(assetmapperPath)
		var b strings.Builder
		b.WriteString("[]" + amPkg + ".Root{\n")
		for _, d := range decls {
			fmt.Fprintf(&b, "{FS: %s.%s, Dir: %q},\n", g.ImportPkg(d.pkg), d.varName, d.dir)
		}
		b.WriteString("}")
		v := g.Var("assetCompiled")
		g.Node(&gen.Call{
			Base: gen.Base{Phase: gen.PhaseWire, Origin: gen.Origin{Pos: decls[0].pos}},
			Var:  v,
			Fn:   amPkg + ".Build",
			Args: []string{b.String(), "nil"},
			Err:  gen.ErrReturn,
		})
		return v, nil
	})

	if as.funcs != nil {
		as.funcs.ContributeFuncs(contributedFuncs, func(g *gen.Gen) (string, diag.Diagnostics) {
			expr, ds, ok := g.InstancePath(compiledPath)
			if !ok {
				return "", ds
			}
			return expr + ".FuncMap()", ds
		})
	}

	return as.host.EmitHandle(g, urlPrefix, as.sortedDecls()[0].pos, func() (string, diag.Diagnostics) {
		expr, ds, ok := g.InstancePath(compiledPath)
		if !ok {
			return "nil", ds
		}
		return expr + ".Handler()", ds
	})
}

// Tree locates one declared asset tree on disk.
type Tree struct {
	SrcDir string // directory of the file declaring the embed
	Dir    string // subdirectory inside the FS ("assets" by default)
}

// Trees returns the declared trees in declaration order, for tooling
// that operates on the sources behind the directive (vendoring).
func (as *Assets) Trees() []Tree {
	decls := as.sortedDecls()
	out := make([]Tree, len(decls))
	for i, d := range decls {
		out[i] = Tree{SrcDir: d.srcDir, Dir: d.dir}
	}
	return out
}

func (as *Assets) sortedDecls() []*assetNode {
	decls := append([]*assetNode(nil), as.decls...)
	sort.Slice(decls, func(i, j int) bool {
		a, b := decls[i].pos, decls[j].pos
		if a.Filename != b.Filename {
			return a.Filename < b.Filename
		}
		return a.Line < b.Line
	})
	return decls
}

// Validate compile-checks the declared trees during wiring: the flat
// union first (fabrik's rule - a path provided twice is an error, so
// diagnostics point at both declarations), then the library's own
// pipeline over the union.
func (as *Assets) Validate(*gen.Gen) diag.Diagnostics {
	if len(as.decls) == 0 {
		return nil
	}
	decls := as.sortedDecls()
	var ds diag.Diagnostics

	owner := map[string]*assetNode{}
	var imOwner *assetNode
	collided := false
	for _, d := range decls {
		tree, err := fs.Sub(as.treeFS(d.srcDir), d.dir)
		if err != nil {
			continue
		}
		walkErr := fs.WalkDir(tree, ".", func(p string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return err
			}
			if p == assetmapper.ImportmapFilename {
				if imOwner != nil {
					ds.Error(d.pos, fmt.Sprintf("importmap.json is already provided by the tree at %s", imOwner.pos),
						"only one asset tree may carry the importmap")
					collided = true
					return nil
				}
				imOwner = d
				return nil
			}
			if first, dup := owner[p]; dup {
				ds.Error(d.pos, fmt.Sprintf("asset %q is already provided by the tree at %s", p, first.pos),
					"each asset path belongs to exactly one tree")
				collided = true
				return nil
			}
			owner[p] = d
			return nil
		})
		// Let the library check below report unreadable trees.
		_ = walkErr
	}

	if !collided {
		roots := make([]assetmapper.Root, len(decls))
		for i, d := range decls {
			roots[i] = assetmapper.Root{FS: as.treeFS(d.srcDir), Dir: d.dir}
		}
		if err := assetmapper.Check(roots, nil); err != nil {
			ds.Error(decls[0].pos, err.Error(),
				"assets are compiled and checked at generation time; fix the tree and rerun")
		}
	}
	return ds
}

// MissingHint explains that compiled assets are injected by pointer.
func (as *Assets) MissingHint(ty types.Type) (string, bool) {
	if types.TypeString(types.Unalias(ty), nil) != assetmapperPath+".Compiled" {
		return "", false
	}
	return "compiled assets are injected as pointers; take *assetmapper.Compiled", true
}
