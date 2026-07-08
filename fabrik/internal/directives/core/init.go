package core

import (
	"fmt"
	"go/token"
	"go/types"
	"strings"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

// Init is the //fabrik:init directive.
type Init struct{}

// NewInit returns an Init directive for one run.
func NewInit() *Init { return &Init{} }

func (*Init) Name() string { return "init" }

func (*Init) Meta() gen.Meta {
	return gen.Meta{
		Synopsis: "Setup function called at startup",
		Doc: "**`//fabrik:init`**\n\n" +
			"Marks a setup function that the generated `run()` calls early, " +
			"in source order. Use it for process-level " +
			"setup like installing the default `slog` logger. It may take a " +
			"`context.Context`; a returned `error` aborts startup.\n\n" +
			"```go\n//fabrik:init\nfunc InitLogger() {\n\tslog.SetDefault(...)\n}\n```",
		Example: "//fabrik:init",
	}
}

type initNode struct {
	pos token.Position

	fn         string
	pkg        *types.Package
	params     []param
	returnsErr bool
}

func (i *Init) Parse(a gen.Annotation) (any, diag.Diagnostics) {
	_, ds := gen.ParseArgs(a, i.Meta())
	if ds.HasFatal() {
		return nil, ds
	}
	return &initNode{pos: a.Pos}, ds
}

func (i *Init) Check(n any, t gen.Typed) diag.Diagnostics {
	nd := n.(*initNode)
	var ds diag.Diagnostics

	fn, ok := t.Target.(*types.Func)
	if !ok {
		ds.Error(nd.pos, "//fabrik:init must be on a function", "")
		return ds
	}
	sig := fn.Signature()
	if sig.Recv() != nil {
		ds.Error(nd.pos, fmt.Sprintf("//fabrik:init must be on a package-level function (func %s is a method)", fn.Name()),
			"move the setup function out of the method set")
		return ds
	}
	if fn.Name() == "init" {
		ds.Error(nd.pos, "//fabrik:init cannot be on Go's init function (it cannot be called by name)",
			"rename it, e.g. func InitLogger()")
		return ds
	}

	results := sig.Results()
	switch {
	case results.Len() == 0:
	case results.Len() == 1 && isErrorType(results.At(0).Type()):
		nd.returnsErr = true
	default:
		ds.Error(nd.pos, fmt.Sprintf("init %s must return nothing or error", fn.Name()),
			"example: func InitTracing(ctx context.Context) error")
		return ds
	}

	// Inits run before providers; only context.Context is available.
	for j := 0; j < sig.Params().Len(); j++ {
		v := sig.Params().At(j)
		if types.TypeString(types.Unalias(v.Type()), nil) != "context.Context" {
			ds.Error(t.Fset.Position(v.Pos()),
				fmt.Sprintf("init parameter %s must be context.Context (inits run before providers)", v.Name()),
				"move dependency setup into a //fabrik:provider")
			return ds
		}
		nd.params = append(nd.params, param{t: v.Type(), pos: t.Fset.Position(v.Pos())})
	}

	nd.fn = fn.Name()
	nd.pkg = fn.Pkg()
	return ds
}

func (i *Init) Emit(n any, g *gen.Gen) diag.Diagnostics {
	nd := n.(*initNode)
	args, ds := resolveParams(g, nd.params)
	call := fmt.Sprintf("%s.%s(%s)", g.ImportPkg(nd.pkg), nd.fn, strings.Join(args, ", "))
	if nd.returnsErr {
		g.Stmt(gen.PhaseInit, "if err := %s; err != nil {\nreturn err\n}", call)
	} else {
		g.Stmt(gen.PhaseInit, "%s", call)
	}
	return ds
}
