package directive

import (
	"fmt"
	"go/token"
	"go/types"
	"strings"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

const runtimeConfigPath = jobsPath + ".RuntimeConfig"

// Worker implements //fabrik:jobs:worker.
type Worker struct {
	b     *builder
	first *token.Position
}

type workerNode struct {
	pos        token.Position
	fn         string
	pkg        *types.Package
	fset       *token.FileSet
	deps       []types.Type
	returnsErr bool
}

func (*Worker) Name() string { return "jobs:worker" }

func (*Worker) Meta() gen.Meta {
	return gen.Meta{
		Synopsis: "Host the jobs worker in this binary",
		Doc: "**`//fabrik:jobs:worker`**\n\n" +
			"Declared on a package function `func(deps...) jobs.RuntimeConfig` (or " +
			"`(jobs.RuntimeConfig, error)`): it marks that this binary hosts the jobs " +
			"worker. The generated `run()` calls `jobs.Run` with the returned config and " +
			"drains it on shutdown. Dependencies (such as a `*JobsConfig`) are injected " +
			"like a provider. Requires at least one `//fabrik:job` or `//fabrik:cron`; one " +
			"per app; a binary that only enqueues jobs omits it. It replaces a hand-written " +
			"`//fabrik:hook start` that ran `jobs.NewWorker(mgr, ...).Start(ctx)`.\n\n" +
			"```go\n//fabrik:jobs:worker\nfunc JobsWorker(cfg *JobsConfig) jobs.RuntimeConfig {\n" +
			"\treturn jobs.RuntimeConfig{Worker: jobs.WorkerConfig{Concurrency: cfg.Concurrency}, RunScheduler: true}\n}\n```",
		Example: "//fabrik:jobs:worker",
	}
}

func (w *Worker) Parse(a gen.Annotation) (any, diag.Diagnostics) {
	_, ds := gen.ParseArgs(a, w.Meta())
	if ds.HasFatal() {
		return nil, ds
	}
	return &workerNode{pos: a.Pos}, ds
}

func (w *Worker) Check(n any, t gen.Typed) diag.Diagnostics {
	nd := n.(*workerNode)
	fn, sig, ds := funcSig(nd.pos, t, "//fabrik:jobs:worker")
	if ds.HasFatal() {
		return ds
	}
	res := sig.Results()
	switch {
	case res.Len() == 1 && typePath(res.At(0).Type()) == runtimeConfigPath:
		nd.returnsErr = false
	case res.Len() == 2 && typePath(res.At(0).Type()) == runtimeConfigPath && typePath(res.At(1).Type()) == "error":
		nd.returnsErr = true
	default:
		ds.Error(nd.pos, fmt.Sprintf("//fabrik:jobs:worker function %s must return jobs.RuntimeConfig or (jobs.RuntimeConfig, error)", fn.Name()),
			"example: func JobsWorker(cfg *JobsConfig) jobs.RuntimeConfig")
		return ds
	}
	for i := 0; i < sig.Params().Len(); i++ {
		nd.deps = append(nd.deps, sig.Params().At(i).Type())
	}
	if w.first != nil {
		ds.Error(nd.pos, "duplicate //fabrik:jobs:worker", fmt.Sprintf("first declared at %s", *w.first))
		return ds
	}
	w.first = &nd.pos
	nd.fn = fn.Name()
	nd.pkg = fn.Pkg()
	nd.fset = t.Fset
	return ds
}

func (w *Worker) Emit(n any, g *gen.Gen) diag.Diagnostics {
	nd := n.(*workerNode)
	var ds diag.Diagnostics

	if len(w.b.jobs) == 0 && len(w.b.crons) == 0 {
		ds.Error(nd.pos, "//fabrik:jobs:worker has no jobs to run",
			"declare at least one //fabrik:job or //fabrik:cron handler, or drop the worker")
		return ds
	}
	w.b.ensure(g)
	mgr, mds, ok := g.InstancePath(managerPath)
	ds = append(ds, mds...)
	if !ok {
		return ds
	}

	deps, dds := resolveDeps(g, nd.pos, nd.deps)
	ds = append(ds, dds...)
	cfgCall := g.ImportPkg(nd.pkg) + "." + nd.fn + "(" + strings.Join(deps, ", ") + ")"

	jobsImport := g.Import(jobsPath)
	ctx := g.Context()
	waits := g.RequireShutdownEnvelope()
	drain := g.Var("jobsDrain")

	runArg := cfgCall
	if nd.returnsErr {
		cfgVar := g.Var("jobsRuntime")
		g.Node(&gen.Call{
			Base: gen.Base{Phase: gen.PhaseStart, Origin: gen.Origin{Pos: nd.pos}},
			Var:  cfgVar,
			Fn:   g.ImportPkg(nd.pkg) + "." + nd.fn,
			Args: deps,
			Err:  gen.ErrReturn,
		})
		runArg = cfgVar
	}
	g.Node(&gen.Call{
		Base: gen.Base{Phase: gen.PhaseStart, Origin: gen.Origin{Pos: nd.pos}},
		Var:  drain,
		Fn:   jobsImport + ".Run",
		Args: []string{ctx, mgr, runArg},
		Err:  gen.ErrReturn,
	})
	g.Node(&gen.Raw{
		Base:  gen.Base{Phase: gen.PhaseStart, Origin: gen.Origin{Pos: nd.pos}, Uses: []string{drain, waits}},
		Lines: []string{fmt.Sprintf("%s = append(%s, %s)", waits, waits, drain)},
	})
	return ds
}
