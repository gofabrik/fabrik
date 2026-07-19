// Package directive implements the fabrik:cli:command generator directive.
package directive

import (
	"fmt"
	"go/token"
	"go/types"
	"strings"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

const cliCommandPtr = "*github.com/gofabrik/fabrik/cli.Command"

// New returns the cli:command directive.
func New() *Command { return &Command{} }

type commandNode struct {
	pos      token.Position
	fn       string
	pkg      *types.Package
	depTypes []types.Type
}

// Command implements the cli:command directive.
type Command struct{}

func (*Command) Name() string { return "cli:command" }

func (*Command) Meta() gen.Meta {
	return gen.Meta{
		Synopsis: "CLI command factory",
		Doc: "**`//fabrik:cli:command`**\n\n" +
			"Declared on a package factory `func(deps...) *cli.Command`: the parameters are " +
			"injected dependencies like a provider, and the function returns a command built " +
			"with the cli library. The generator resolves the dependencies, calls the factory, " +
			"and registers the returned command so `app <name>` dispatches to it.\n\n" +
			"Declaring any `//fabrik:cli:command` makes the generated `run()` dispatch commands: a " +
			"subcommand runs its factory's command, while a bare invocation runs prepare and start " +
			"hooks without starting injected runtimes. HTTP servers and jobs runners must " +
			"be injected into and started by a command. Because `run()` returns an exit code, " +
			"`main` must be " +
			"`func main() { os.Exit(run()) }`; adding the first command to an app whose `main` still " +
			"uses `if err := run(); err != nil` will not compile until `main` is updated.\n\n" +
			"```go\n//fabrik:cli:command\nfunc GreetCommand(g *Greeter) *cli.Command { ... }\n```",
		Example: "//fabrik:cli:command",
	}
}

func (c *Command) Parse(a gen.Annotation) (any, diag.Diagnostics) {
	_, ds := gen.ParseArgs(a, c.Meta())
	if ds.HasFatal() {
		return nil, ds
	}
	return &commandNode{pos: a.Pos}, ds
}

func (c *Command) Check(n any, t gen.Typed) diag.Diagnostics {
	nd := n.(*commandNode)
	fn, sig, ds := funcSig(nd.pos, t)
	if ds.HasFatal() {
		return ds
	}
	res := sig.Results()
	if res.Len() != 1 || typePath(res.At(0).Type()) != cliCommandPtr {
		ds.Error(nd.pos, fmt.Sprintf("command %s has the wrong signature", fn.Name()),
			"want func(deps...) *cli.Command")
		return ds
	}
	// Dependencies resolve during Emit, when the object graph is available.
	p := sig.Params()
	for i := 0; i < p.Len(); i++ {
		nd.depTypes = append(nd.depTypes, p.At(i).Type())
	}
	nd.fn = fn.Name()
	nd.pkg = fn.Pkg()
	return ds
}

func (c *Command) Emit(n any, g *gen.Gen) diag.Diagnostics {
	nd := n.(*commandNode)
	if nd.fn == "" {
		return nil
	}
	deps, ds := resolveDeps(g, nd.pos, nd.depTypes)
	call := g.ImportPkg(nd.pkg) + "." + nd.fn + "(" + strings.Join(deps, ", ") + ")"
	g.AddCommand(call)
	return ds
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

func resolveDeps(g *gen.Gen, pos token.Position, depTypes []types.Type) ([]string, diag.Diagnostics) {
	var ds diag.Diagnostics
	out := make([]string, 0, len(depTypes))
	for _, dt := range depTypes {
		e, dds, ok := g.Instance(dt, "")
		ds = append(ds, dds...)
		if !ok {
			ds.Error(pos, fmt.Sprintf("no provider for %s", g.TypeExpr(dt)),
				fmt.Sprintf("add a //fabrik:provider returning %s", g.TypeExpr(dt)))
			out = append(out, "nil")
			continue
		}
		out = append(out, e)
	}
	return out, ds
}

func typePath(t types.Type) string {
	t = types.Unalias(t)
	if p, ok := t.(*types.Pointer); ok {
		return "*" + typePath(p.Elem())
	}
	return types.TypeString(t, nil)
}
