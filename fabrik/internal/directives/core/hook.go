package core

import (
	"fmt"
	"go/token"
	"go/types"

	cfgdir "github.com/gofabrik/fabrik/config/directive"
	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

// Hook is the //fabrik:hook directive.
type Hook struct {
	cfg *cfgdir.Config
}

// NewHook returns a Hook directive for one run.
func NewHook(cfg *cfgdir.Config) *Hook { return &Hook{cfg: cfg} }

func (*Hook) Name() string { return "hook" }

func (*Hook) Meta() gen.Meta {
	return gen.Meta{
		Synopsis: "Lifecycle hook function: setup",
		Doc: "**`//fabrik:hook setup`**\n\n" +
			"Marks a function the generated assembly calls at a named point of " +
			"the app lifecycle: `config -> setup -> providers -> middleware -> " +
			"routes`. Hookable phase:\n\n" +
			"- `setup` - after config, before providers. Process-level setup " +
			"(logger, runtime tuning); parameters may be a leading " +
			"`context.Context` and pointers to //fabrik:config structs. With " +
			"commands, setup runs at the start of every command's build " +
			"function, under the command context.\n\n" +
			"Hooks run in source order and must be independent. " +
			"A returned `error` aborts startup. Pre-intake work such as " +
			"migrations belongs to an explicit command that injects what it " +
			"needs; runtime processes are startable values a command runs.\n\n" +
			"```go\n//fabrik:hook setup\nfunc InitLogger(cfg *Log) error {\n\tslog.SetDefault(...)\n\treturn nil\n}\n```",
		Example: "//fabrik:hook setup",
		Pos: []gen.PosSpec{
			{Name: "PHASE", Kind: gen.KindEnum, Values: []string{"setup"}},
		},
		Tier: gen.TierHook,
	}
}

type hookNode struct {
	pos   token.Position
	phase string

	fn         string
	pkg        *types.Package
	params     []param
	returnsErr bool
}

func (h *Hook) Parse(a gen.Annotation) (any, diag.Diagnostics) {
	args, ds := gen.ParseArgs(a, h.Meta())
	if len(args.Pos) < 1 {
		return nil, ds
	}
	phase := args.Pos[0]
	switch phase.Text {
	case "setup":
	default:
		ds.Error(a.ArgPos(phase.Col), fmt.Sprintf("unknown lifecycle phase %q", phase.Text),
			"the hookable phase is setup (after config, before providers); pre-intake and runtime work belong to commands")
	}
	if ds.HasFatal() {
		return nil, ds
	}
	return &hookNode{pos: a.Pos, phase: phase.Text}, ds
}

func (h *Hook) Check(n any, t gen.Typed) diag.Diagnostics {
	nd := n.(*hookNode)
	var ds diag.Diagnostics

	fn, ok := t.Target.(*types.Func)
	if !ok {
		ds.Error(nd.pos, "//fabrik:hook must be on a function", "")
		return ds
	}
	sig := fn.Signature()
	if sig.Recv() != nil {
		ds.Error(nd.pos, fmt.Sprintf("//fabrik:hook must be on a package-level function (func %s is a method)", fn.Name()),
			"move the hook out of the method set")
		return ds
	}
	if fn.Name() == "init" {
		ds.Error(nd.pos, "//fabrik:hook cannot be on Go's init function (it cannot be called by name)",
			"rename it, e.g. func InitLogger()")
		return ds
	}
	if sig.TypeParams().Len() > 0 {
		ds.Error(nd.pos, fmt.Sprintf("hook %s cannot be generic (generated code cannot infer type arguments)", fn.Name()),
			"declare a concrete hook function")
		return ds
	}

	results := sig.Results()
	switch {
	case results.Len() == 0:
	case results.Len() == 1 && isErrorType(results.At(0).Type()):
		nd.returnsErr = true
	default:
		ds.Error(nd.pos, fmt.Sprintf("hook %s must return nothing or error", fn.Name()),
			"example: func MigrateDB(ctx context.Context) error")
		return ds
	}

	for j := 0; j < sig.Params().Len(); j++ {
		v := sig.Params().At(j)
		// A context parameter must be first.
		if j > 0 && gen.IsContext(v.Type()) {
			ds.Error(t.Fset.Position(v.Pos()), fmt.Sprintf("hook %s takes context.Context after other parameters", fn.Name()),
				"a hook's context must be the first parameter")
			return ds
		}
		nd.params = append(nd.params, param{t: v.Type(), pos: t.Fset.Position(v.Pos())})
	}

	nd.fn = fn.Name()
	nd.pkg = fn.Pkg()
	return ds
}

func (h *Hook) Emit(n any, g *gen.Gen) diag.Diagnostics {
	nd := n.(*hookNode)

	emit := func() diag.Diagnostics {
		// Setup hooks can consume only context and config.
		args, ds := resolveArgs(g, h.cfg, nd.params,
			func(pr param) (string, diag.Diagnostics, bool) {
				if !h.cfg.IsConfig(pr.t) {
					return "", nil, false
				}
				return g.Instance(pr.t, "")
			},
			func(param) (string, string) {
				return "setup hooks run before providers; parameters must be context.Context or //fabrik:config structs",
					"construct the resource in a //fabrik:provider and inject it into a command"
			})
		errStyle := gen.ErrNone
		if nd.returnsErr {
			errStyle = gen.ErrInline
		}
		g.Node(&gen.Call{
			Base: gen.Base{Phase: gen.PhaseSetup, Origin: gen.Origin{Pos: nd.pos}},
			Fn:   g.ImportPkg(nd.pkg) + "." + nd.fn,
			Args: args,
			Err:  errStyle,
		})
		return ds
	}
	if g.ScopeCount() > 0 {
		// Each command runs setup with config resolved in its own scope.
		g.ScopePrologue(emit)
		return nil
	}
	return emit()
}
