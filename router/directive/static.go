package directive

import (
	"fmt"
	"go/token"
	"go/types"
	"strconv"
	"strings"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

// Static is the //fabrik:http:static directive.
type Static struct {
	host *Host
}

// NewStatic returns a Static directive for one run.
func NewStatic(host *Host) *Static { return &Static{host: host} }

func (*Static) Name() string { return "http:static" }

// PrepareNode binds the HTTP server for static routes.
func (s *Static) PrepareNode(_ any, g *gen.Gen) { s.host.BindHTTPServer(g) }

func (*Static) Meta() gen.Meta {
	return gen.Meta{
		Synopsis: "Serve an embedded file tree: /prefix [dir=sub]",
		Doc: "**`//fabrik:http:static /prefix [dir=sub]`**\n\n" +
			"Declared on an exported `embed.FS` variable: serves its files " +
			"under the prefix. `dir=` strips the embedded directory name so " +
			"URLs do not repeat it.\n\n" +
			"```go\n//fabrik:http:static /assets dir=assets\n//go:embed assets\nvar Assets embed.FS\n```",
		Example: "//fabrik:http:static /assets dir=assets",
		Tier:    gen.TierBind,
		Pos: []gen.PosSpec{
			{Name: "PREFIX", Kind: gen.KindFreeform},
		},
		Attrs: []gen.AttrSpec{
			{Key: "dir", Kind: gen.KindFreeform},
		},
	}
}

type staticNode struct {
	prefix string
	dir    string
	pos    token.Position

	varName string
	pkg     *types.Package
}

func (s *Static) Parse(a gen.Annotation) (any, diag.Diagnostics) {
	args, ds := gen.ParseArgs(a, s.Meta())
	if len(args.Pos) < 1 {
		return nil, ds
	}
	prefix := args.Pos[0]
	switch {
	case !strings.HasPrefix(prefix.Text, "/") || prefix.Text == "/":
		ds.Error(a.ArgPos(prefix.Col), fmt.Sprintf("static prefix must be a non-root path starting with %q (got %q)", "/", prefix.Text),
			"example: //fabrik:http:static /assets")
	case strings.ContainsAny(prefix.Text, "{}"):
		ds.Error(a.ArgPos(prefix.Col), fmt.Sprintf("static prefix cannot contain wildcards (got %q)", prefix.Text),
			"the prefix is a literal path under which files are served")
	}
	nd := &staticNode{prefix: strings.TrimSuffix(prefix.Text, "/"), pos: a.Pos}
	if dir, ok := args.Attr["dir"]; ok {
		nd.dir = dir.Text
	}
	if ds.HasFatal() {
		return nil, ds
	}
	return nd, ds
}

func (s *Static) Check(n any, t gen.Typed) diag.Diagnostics {
	nd := n.(*staticNode)
	var ds diag.Diagnostics

	v, ok := t.Target.(*types.Var)
	if !ok {
		ds.Error(nd.pos, "//fabrik:http:static must be on a single package-level variable", "")
		return ds
	}
	if types.TypeString(types.Unalias(v.Type()), nil) != "embed.FS" {
		ds.Error(nd.pos, fmt.Sprintf("//fabrik:http:static must be on an embed.FS variable (%s is %s)", v.Name(), types.TypeString(v.Type(), types.RelativeTo(v.Pkg()))),
			"declare it with //go:embed as: var Assets embed.FS")
		return ds
	}
	if !v.Exported() {
		ds.Error(nd.pos, fmt.Sprintf("embed.FS variable %s is unexported", v.Name()),
			"fabrik wires from package main; export the variable")
		return ds
	}

	nd.varName = v.Name()
	nd.pkg = v.Pkg()
	return ds
}

func (s *Static) Emit(n any, g *gen.Gen) diag.Diagnostics {
	nd := n.(*staticNode)

	pattern := nd.prefix + "/"
	if d, ok := s.host.routes.add(pattern, nd.pos); !ok {
		return diag.Diagnostics{d}
	}

	s.host.record(func(g *gen.Gen) diag.Diagnostics {
		r := g.Singleton(routerPath, "r", g.Import(routerPath)+".New()")
		httpPkg := g.Import("net/http")
		fsExpr := g.ImportPkg(nd.pkg) + "." + nd.varName
		if nd.dir != "" {
			fsPkg := g.Import("io/fs")
			v := g.Var(nd.pkg.Name() + nd.varName)
			g.Node(&gen.Call{
				Base: gen.Base{
					Phase:  gen.PhaseWire,
					Origin: gen.Origin{Pos: nd.pos},
					Label:  "embedded " + nd.varName + ", served under " + nd.prefix,
				},
				Var:  v,
				Fn:   fsPkg + ".Sub",
				Args: []string{fsExpr, strconv.Quote(nd.dir)},
				Err:  gen.ErrReturn,
			})
			fsExpr = v
		}
		g.Node(&gen.Route{
			Base:    gen.Base{Phase: gen.PhaseRegister, Origin: gen.Origin{Pos: nd.pos}},
			Router:  r,
			Kind:    gen.RouteHandle,
			Pattern: pattern,
			Handler: httpPkg + ".StripPrefix(" + strconv.Quote(nd.prefix) + ", " + httpPkg + ".FileServerFS(" + fsExpr + "))",
		})
		return nil
	})
	return nil
}
