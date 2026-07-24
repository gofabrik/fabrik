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
	cfg  *cfgdir.Config
	seen map[string]token.Position
}

// NewHook returns a Hook directive for one run.
func NewHook(cfg *cfgdir.Config) *Hook {
	return &Hook{cfg: cfg, seen: map[string]token.Position{}}
}

func (*Hook) Name() string { return "hook" }

func (*Hook) Meta() gen.Meta {
	return gen.Meta{
		Synopsis: "Wire hook function: setup",
		Doc: "**`//fabrik:hook setup`**\n\n" +
			"Declares an exported setup hook called after configuration loads " +
			"and before providers are built. It takes `context.Context` first, " +
			"followed by pointers to registered `//fabrik:config` structs, and " +
			"returns one `error`. A hook configures process or build behavior " +
			"from loaded configuration; bounded, cancellation-respecting work " +
			"is admissible, but a hook must not construct dependencies, " +
			"retain goroutines, or own resources needing cleanup - durable " +
			"resources belong to providers. Every declared config becomes " +
			"demand in every selected command scope, loaded once before the " +
			"first hook runs.\n\n" +
			"Hooks run once per selected-command build attempt that reaches " +
			"setup - including attempts where a later provider or the hook " +
			"itself fails; nothing runs for help, completion, parse failures, " +
			"or a failed prerequisite config load. They run sequentially in " +
			"deterministic generated code, but their relative order is " +
			"unspecified. The first error aborts the build without rolling " +
			"back earlier effects, so hooks must be idempotent. Pre-intake " +
			"work such as migrations belongs to an explicit command; runtime " +
			"processes are startable values a command runs.\n\n" +
			"```go\n//fabrik:hook setup\nfunc InitLogger(_ context.Context, cfg *Log) error {\n\tslog.SetDefault(...)\n\treturn nil\n}\n```",
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

	fn     string
	pkg    *types.Package
	params []param
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
		ds.Error(a.ArgPos(phase.Col), fmt.Sprintf("unknown wire-hook point %q", phase.Text),
			"the wire-hook point is setup (config loaded, providers not yet); pre-intake and runtime work belong to commands")
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
	if !fn.Exported() {
		ds.Error(nd.pos, fmt.Sprintf("hook %s is unexported", fn.Name()),
			"generated code lives in package main; export the function")
		return ds
	}
	if sig.Variadic() {
		ds.Error(nd.pos, fmt.Sprintf("hook %s is variadic", fn.Name()),
			"a wire hook's shape is func(ctx context.Context, ...*Config) error with fixed parameters")
		return ds
	}

	results := sig.Results()
	if results.Len() != 1 || !isErrorType(results.At(0).Type()) {
		ds.Error(nd.pos, fmt.Sprintf("hook %s must return exactly error", fn.Name()),
			"example: func ConfigureLogging(ctx context.Context, cfg *LogConfig) error")
		return ds
	}

	if sig.Params().Len() == 0 || !gen.IsContext(sig.Params().At(0).Type()) {
		ds.Error(nd.pos, fmt.Sprintf("hook %s must take context.Context as its first parameter", fn.Name()),
			"example: func ConfigureLogging(ctx context.Context, cfg *LogConfig) error")
		return ds
	}
	for j := 0; j < sig.Params().Len(); j++ {
		v := sig.Params().At(j)
		if j > 0 && gen.IsContext(v.Type()) {
			ds.Error(t.Fset.Position(v.Pos()), fmt.Sprintf("hook %s takes a second context.Context", fn.Name()),
				"a hook's context is exactly its first parameter")
			return ds
		}
		nd.params = append(nd.params, param{t: v.Type(), pos: t.Fset.Position(v.Pos())})
	}

	qual := fn.Pkg().Path() + "." + fn.Name()
	if first, dup := h.seen[qual]; dup {
		ds.Error(nd.pos, fmt.Sprintf("duplicate //fabrik:hook on %s", fn.Name()),
			fmt.Sprintf("first declared at %s", first))
		return ds
	}
	h.seen[qual] = nd.pos

	nd.fn = fn.Name()
	nd.pkg = fn.Pkg()
	return ds
}

func (h *Hook) Emit(n any, g *gen.Gen) diag.Diagnostics {
	nd := n.(*hookNode)

	emit := func() diag.Diagnostics {
		// Config aliases resolve only after all config registrations are checked.
		seenConfigs := map[string]bool{}
		args, ds := resolveArgs(g, h.cfg, nd.params,
			func(pr param) (string, diag.Diagnostics, bool) {
				ct := cfgdir.Canonical(pr.t)
				if !h.cfg.IsConfig(ct) {
					return "", nil, false
				}
				key := types.TypeString(ct, nil)
				if seenConfigs[key] {
					var dup diag.Diagnostics
					dup.Error(pr.pos, fmt.Sprintf("hook %s declares %s twice", nd.fn, key),
						"one parameter per config type; multiple hooks may share a config")
					return "", dup, false
				}
				seenConfigs[key] = true
				return g.Instance(ct, "")
			},
			func(param) (string, string) {
				return "a setup wire hook's parameters are context.Context then //fabrik:config structs; providers do not exist yet",
					"construct the resource in a //fabrik:provider and inject it into a command"
			})
		g.Node(&gen.Call{
			Base: gen.Base{Phase: gen.PhaseSetup, Origin: gen.Origin{Pos: nd.pos}},
			Fn:   g.ImportPkg(nd.pkg) + "." + nd.fn,
			Args: args,
			Err:  gen.ErrInline,
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
