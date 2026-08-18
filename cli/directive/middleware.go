package directive

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"slices"
	"sort"
	"strings"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

const cliHandlerPath = "github.com/gofabrik/fabrik/cli.Handler"

type mwNode struct {
	pos      token.Position
	decl     ast.Node
	name     string
	requires []mwRef

	fn  string
	pkg *types.Package
}

// mwRef retains a requires= reference's source position for diagnostics.
type mwRef struct {
	name string
	pos  token.Position
}

// Middleware implements //fabrik:cli:middleware.
type Middleware struct{ fam *family }

func (*Middleware) Name() string { return "cli:middleware" }

func (*Middleware) Meta() gen.Meta {
	return gen.Meta{
		Synopsis: "Named CLI middleware",
		Doc: "**`//fabrik:cli:middleware name=<token> [requires=a,b]`**\n\n" +
			"Declared on an exported `func(cli.Handler) cli.Handler`: registers " +
			"the function under a name that `middleware=` chains on " +
			"`//fabrik:cli:command`, `//fabrik:cli:group`, and `//fabrik:cli:root` " +
			"reference. Chains attach in declaration order; the cli library " +
			"applies root middleware outermost, then each ancestor, then the " +
			"command's own. `requires=` lists middleware that must run earlier " +
			"through the root, an ancestor group or command, or an earlier entry " +
			"in the same `middleware=` list; unmet requirements fail wiring. " +
			"Unreferenced declarations warn.\n\n" +
			"```go\n//fabrik:cli:middleware name=confirm\nfunc Confirm(next cli.Handler) cli.Handler { ... }\n```",
		Example: "//fabrik:cli:middleware name=confirm",
		Tier:    gen.TierBind,
		Attrs: []gen.AttrSpec{
			{Key: "name", Kind: gen.KindFreeform},
			{Key: "requires", Kind: gen.KindCLIMiddlewareRef},
		},
	}
}

func (m *Middleware) Parse(a gen.Annotation) (any, diag.Diagnostics) {
	args, ds := gen.ParseArgs(a, m.Meta())
	if ds.HasFatal() {
		return nil, ds
	}
	name, ok := args.Attr["name"]
	if !ok {
		ds.Error(a.Pos, "//fabrik:cli:middleware needs name=", "example: "+m.Meta().Example)
		return nil, ds
	}
	if !tokenRE.MatchString(name.Text) {
		ds.Error(a.ArgPos(name.Col), fmt.Sprintf("invalid CLI token %q", name.Text),
			"names are lowercase kebab-case: [a-z0-9]+(-[a-z0-9]+)*")
		return nil, ds
	}
	nd := &mwNode{pos: a.Pos, decl: a.Decl, name: name.Text}
	if req, ok := args.Attr["requires"]; ok {
		refs, rds := splitMWRefs(a, req)
		ds = append(ds, rds...)
		if ds.HasFatal() {
			return nil, ds
		}
		nd.requires = refs
	}
	return nd, ds
}

// splitMWRefs parses requires= while retaining each token's source position.
func splitMWRefs(a gen.Annotation, attr gen.Arg) ([]mwRef, diag.Diagnostics) {
	var ds diag.Diagnostics
	var out []mwRef
	offset := 0
	for part := range strings.SplitSeq(attr.Text, ",") {
		pos := a.ArgPos(attr.Col + offset)
		offset += len(part) + 1
		if !tokenRE.MatchString(part) {
			ds.Error(pos, fmt.Sprintf("invalid CLI token %q in requires=", part),
				"references are single lowercase kebab-case tokens")
			return nil, ds
		}
		out = append(out, mwRef{name: part, pos: pos})
	}
	return out, ds
}

// earlierNames returns root and ancestor middleware in runtime order, excluding path itself.
func (f *family) earlierNames(path []string) []string {
	var out []string
	if f.root != nil {
		out = append(out, f.root.middleware...)
	}
	type ancestor struct {
		depth int
		mws   []string
	}
	var ancestors []ancestor
	for _, gr := range f.groups {
		if len(gr.path) < len(path) && slices.Equal(gr.path, path[:len(gr.path)]) {
			ancestors = append(ancestors, ancestor{depth: len(gr.path), mws: gr.middleware})
		}
	}
	for _, cmd := range f.commands {
		if len(cmd.path) < len(path) && slices.Equal(cmd.path, path[:len(cmd.path)]) {
			ancestors = append(ancestors, ancestor{depth: len(cmd.path), mws: cmd.middleware})
		}
	}
	sort.SliceStable(ancestors, func(i, j int) bool { return ancestors[i].depth < ancestors[j].depth })
	for _, a := range ancestors {
		out = append(out, a.mws...)
	}
	return out
}

// validateRequires reports known prerequisites absent from base or earlier in chain.
func (f *family) validateRequires(pos token.Position, chain, base []string) diag.Diagnostics {
	var ds diag.Diagnostics
	seen := make(map[string]bool, len(base)+len(chain))
	for _, name := range base {
		seen[name] = true
	}
	for _, name := range chain {
		if mw, ok := f.middlewares[name]; ok {
			for _, req := range mw.requires {
				if _, known := f.middlewares[req.name]; !known {
					continue
				}
				if !seen[req.name] {
					ds.Error(pos, fmt.Sprintf("middleware %q requires=%s, which does not run earlier in this chain", name, req.name),
						fmt.Sprintf("add %s before %s in the middleware= list, or attach it to the root or an ancestor group", req.name, name))
				}
			}
		}
		seen[name] = true
	}
	return ds
}

// checkRequiresDecls validates targets even when their middleware is unreferenced.
func (f *family) checkRequiresDecls() diag.Diagnostics {
	var ds diag.Diagnostics
	for _, mw := range f.mwOrder {
		for _, req := range mw.requires {
			if _, ok := f.middlewares[req.name]; !ok {
				ds.Error(req.pos, fmt.Sprintf("requires=%s: unknown CLI middleware %q", req.name, req.name),
					knownMiddlewareHelp(f))
			}
		}
	}
	return ds
}

func (m *Middleware) Check(n any, t gen.Typed) diag.Diagnostics {
	nd := n.(*mwNode)
	var ds diag.Diagnostics
	fn, ok := t.Target.(*types.Func)
	if !ok {
		ds.Error(nd.pos, "//fabrik:cli:middleware must be on a function", "")
		return ds
	}
	if !fn.Exported() {
		ds.Error(nd.pos, fmt.Sprintf("//fabrik:cli:middleware function %s must be exported", fn.Name()),
			"generated code calls it as a package-qualified symbol; capitalize the name")
		return ds
	}
	sig := fn.Signature()
	if sig.TypeParams().Len() > 0 {
		ds.Error(nd.pos, fmt.Sprintf("//fabrik:cli:middleware function %s cannot be generic", fn.Name()),
			"generated code cannot instantiate type parameters; declare a concrete function")
		return ds
	}
	if sig.Recv() != nil || sig.Params().Len() != 1 || sig.Results().Len() != 1 ||
		typePath(sig.Params().At(0).Type()) != cliHandlerPath ||
		typePath(sig.Results().At(0).Type()) != cliHandlerPath {
		ds.Error(nd.pos, fmt.Sprintf("middleware %s has the wrong signature", fn.Name()),
			"want func(next cli.Handler) cli.Handler")
		return ds
	}
	if first, dup := m.fam.middlewares[nd.name]; dup {
		ds.Error(nd.pos, fmt.Sprintf("duplicate //fabrik:cli:middleware name %q", nd.name),
			fmt.Sprintf("first declared at %s", first.pos))
		return ds
	}
	nd.fn = fn.Name()
	nd.pkg = fn.Pkg()
	m.fam.middlewares[nd.name] = nd
	m.fam.mwOrder = append(m.fam.mwOrder, nd)
	return ds
}

func (*Middleware) Emit(any, *gen.Gen) diag.Diagnostics { return nil }

// resolveMiddleware resolves names in order and marks them referenced.
func (f *family) resolveMiddleware(g *gen.Gen, pos token.Position, names []string) ([]string, diag.Diagnostics) {
	var ds diag.Diagnostics
	var out []string
	for _, name := range names {
		mw, ok := f.middlewares[name]
		if !ok {
			// Continue so known references and all unknown names are recorded.
			ds.Error(pos, fmt.Sprintf("unknown CLI middleware %q", name), knownMiddlewareHelp(f))
			continue
		}
		f.mwReferenced[name] = true
		out = append(out, g.ImportPkg(mw.pkg)+"."+mw.fn)
	}
	if ds.HasFatal() {
		return nil, ds
	}
	return out, ds
}

func knownMiddlewareHelp(f *family) string {
	if len(f.middlewares) == 0 {
		return "declare one with //fabrik:cli:middleware name=..."
	}
	names := make([]string, 0, len(f.middlewares))
	for name := range f.middlewares {
		names = append(names, name)
	}
	sort.Strings(names)
	return "known: " + strings.Join(names, ", ")
}

func splitMiddleware(a gen.Annotation, attr gen.Arg) ([]string, diag.Diagnostics) {
	var ds diag.Diagnostics
	var out []string
	for name := range strings.SplitSeq(attr.Text, ",") {
		if !tokenRE.MatchString(name) {
			ds.Error(a.ArgPos(attr.Col), fmt.Sprintf("invalid CLI token %q in middleware=", name),
				"references are single lowercase kebab-case tokens")
			return nil, ds
		}
		out = append(out, name)
	}
	return out, ds
}
