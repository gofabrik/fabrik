// Package directive implements the fabrik:cli:command generator directive.
package directive

import (
	"fmt"
	"go/ast"
	"go/doc"
	"go/token"
	"go/types"
	"strings"
	"unicode"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

const cliContextPath = "github.com/gofabrik/fabrik/cli.Context"

// New returns the cli:command directive.
func New() *Command { return &Command{byName: map[string]token.Position{}} }

type commandNode struct {
	pos  token.Position
	doc  string
	name string
	help string

	fn       string
	pkg      *types.Package
	depTypes []types.Type
}

// Command implements the cli:command directive.
type Command struct {
	byName map[string]token.Position
}

func (*Command) Name() string { return "cli:command" }

func (*Command) Meta() gen.Meta {
	return gen.Meta{
		Synopsis: "CLI command function",
		Doc: "**`//fabrik:cli:command`**\n\n" +
			"Declared on a package function `func(ctx cli.Context, deps...) error`: the first " +
			"parameter is the invocation context, the rest are injected dependencies like a " +
			"provider. The command name derives from the function name (`ServeHTTP` becomes " +
			"`serve-http`); the help line is the doc comment's first sentence. The generator " +
			"emits a build function that constructs exactly this command's dependencies when " +
			"the command is selected, so help and completion never construct the application.\n\n" +
			"A bare invocation lists the commands. Runtimes are startable injected values " +
			"(`*httpserver.Server`, `*jobs.Runner`) the command starts itself; migrations are " +
			"an injectable `migrations.Sources` the command applies itself. `run()` returns an " +
			"exit code, so `main` is always `func main() { os.Exit(run()) }`.\n\n" +
			"```go\n// Start the HTTP server.\n//\n//fabrik:cli:command\nfunc Serve(ctx cli.Context, server *httpserver.Server) error {\n\treturn server.Run(ctx)\n}\n```",
		Example: "//fabrik:cli:command",
		Tier:    gen.TierBind,
	}
}

func (c *Command) Parse(a gen.Annotation) (any, diag.Diagnostics) {
	_, ds := gen.ParseArgs(a, c.Meta())
	if ds.HasFatal() {
		return nil, ds
	}
	nd := &commandNode{pos: a.Pos}
	if fd, ok := a.Decl.(*ast.FuncDecl); ok && fd.Doc != nil {
		nd.doc = fd.Doc.Text()
	}
	return nd, ds
}

func (c *Command) Check(n any, t gen.Typed) diag.Diagnostics {
	nd := n.(*commandNode)
	fn, sig, ds := funcSig(nd.pos, t)
	if ds.HasFatal() {
		return ds
	}
	res := sig.Results()
	if res.Len() != 1 || typePath(res.At(0).Type()) != "error" {
		ds.Error(nd.pos, fmt.Sprintf("command %s has the wrong signature", fn.Name()),
			"want func(ctx cli.Context, deps...) error")
		return ds
	}
	p := sig.Params()
	if p.Len() == 0 || typePath(p.At(0).Type()) != cliContextPath {
		ds.Error(nd.pos, fmt.Sprintf("command %s must take cli.Context as its first parameter", fn.Name()),
			"want func(ctx cli.Context, deps...) error")
		return ds
	}
	// Dependencies remain unresolved until the selected command is built.
	for i := 1; i < p.Len(); i++ {
		nd.depTypes = append(nd.depTypes, p.At(i).Type())
	}

	nd.name = kebabCase(fn.Name())
	if first, dup := c.byName[nd.name]; dup {
		ds.Error(nd.pos, fmt.Sprintf("duplicate command name %q", nd.name),
			fmt.Sprintf("first declared at %s; rename one function", first))
		return ds
	}
	c.byName[nd.name] = nd.pos

	nd.help = helpFromDoc(nd.doc)
	nd.fn = fn.Name()
	nd.pkg = fn.Pkg()
	return ds
}

func (c *Command) Emit(n any, g *gen.Gen) diag.Diagnostics {
	nd := n.(*commandNode)
	if nd.fn == "" {
		return nil
	}
	scope := g.AddScope("build"+nd.fn, nd.pos, nd.depTypes...)
	g.AddCommandFunc(gen.CommandFunc{
		Name:  nd.name,
		Help:  nd.help,
		Fn:    g.ImportPkg(nd.pkg) + "." + nd.fn,
		Scope: scope,
		Pos:   nd.pos,
	})
	return nil
}

// kebabCase converts a Go identifier to a CLI token while preserving acronym runs.
func kebabCase(name string) string {
	rs := []rune(name)
	var b strings.Builder
	for i, r := range rs {
		if unicode.IsUpper(r) {
			startsWord := i > 0 && (!unicode.IsUpper(rs[i-1]) || (i+1 < len(rs) && unicode.IsLower(rs[i+1])))
			if startsWord {
				b.WriteByte('-')
			}
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// helpFromDoc returns the godoc synopsis without directives or a trailing period.
func helpFromDoc(text string) string {
	var kept []string
	for _, ln := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "fabrik:") {
			continue
		}
		kept = append(kept, ln)
	}
	syn := new(doc.Package).Synopsis(strings.TrimSpace(strings.Join(kept, "\n")))
	return strings.TrimSuffix(syn, ".")
}

func funcSig(pos token.Position, t gen.Typed) (*types.Func, *types.Signature, diag.Diagnostics) {
	var ds diag.Diagnostics
	fn, ok := t.Target.(*types.Func)
	if !ok {
		ds.Error(pos, "//fabrik:cli:command must be on a function", "")
		return nil, nil, ds
	}
	sig := fn.Signature()
	if sig.TypeParams().Len() > 0 || sig.RecvTypeParams().Len() > 0 {
		ds.Error(pos, fmt.Sprintf("//fabrik:cli:command function %s cannot be generic", fn.Name()),
			"declare a concrete function")
		return nil, nil, ds
	}
	if sig.Recv() != nil {
		ds.Error(pos, "//fabrik:cli:command must be a package function, not a method",
			"dependencies are parameters, not receiver fields")
		return nil, nil, ds
	}
	if !fn.Exported() {
		ds.Error(pos, fmt.Sprintf("//fabrik:cli:command function %s must be exported", fn.Name()),
			"generated code calls it as a package-qualified symbol; capitalize the name")
		return nil, nil, ds
	}
	if sig.Variadic() {
		ds.Error(pos, fmt.Sprintf("//fabrik:cli:command function %s cannot be variadic", fn.Name()),
			"declare each dependency as its own parameter")
		return nil, nil, ds
	}
	return fn, sig, ds
}

func typePath(t types.Type) string {
	t = types.Unalias(t)
	if p, ok := t.(*types.Pointer); ok {
		return "*" + typePath(p.Elem())
	}
	return types.TypeString(t, nil)
}
