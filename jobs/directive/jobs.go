// Package directive adds //fabrik:job and //fabrik:cron to the generator.
package directive

import (
	"fmt"
	"go/token"
	"go/types"
	"sort"
	"strings"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
	"github.com/robfig/cron/v3"
)

const (
	jobsPath        = "github.com/gofabrik/fabrik/jobs"
	managerPath     = "*" + jobsPath + ".Manager"
	storePath       = jobsPath + ".Store"
	jobsContextPath = jobsPath + ".Context"
)

// New returns the paired jobs directives.
func New() (*Jobs, *Cron) {
	b := &builder{}
	return &Jobs{b: b}, &Cron{b: b}
}

// builder is state shared between the job and cron directives.
type builder struct {
	jobs       []*jobNode
	crons      []*cronNode
	registered bool
	built      bool
}

type jobNode struct {
	pos      token.Position
	name     string // handler-id: name= or the function name
	kind     string // kind= override, resolved otherwise
	fn       string
	pkg      *types.Package
	fset     *token.FileSet
	msgType  types.Type
	depTypes []types.Type
}

type cronNode struct {
	pos      token.Position
	name     string
	schedule string
	fn       string
	pkg      *types.Package
	fset     *token.FileSet
	depTypes []types.Type
}

func (b *builder) pos() token.Position {
	if len(b.jobs) > 0 {
		return b.jobs[0].pos
	}
	if len(b.crons) > 0 {
		return b.crons[0].pos
	}
	return token.Position{}
}

// Jobs implements //fabrik:job.
type Jobs struct{ b *builder }

func (*Jobs) Name() string { return "job" }

func (*Jobs) Meta() gen.Meta {
	return gen.Meta{
		Synopsis: "Background-job handler: [name=NAME] [kind=KIND]",
		Doc: "**`//fabrik:job [name=NAME] [kind=KIND]`**\n\n" +
			"Declared on a package function `func(ctx, deps..., msg T) error`: " +
			"the first parameter is a `context.Context` (or `jobs.Context` for " +
			"the accessors), the middle parameters are injected dependencies " +
			"like a provider, and the **last** parameter is the message. One " +
			"handler on a message type is a command (`Enqueue`), several are an " +
			"event (`Publish`). All handlers assemble one injected " +
			"`*jobs.Manager` whose only dependency is a `jobs.Store` provider. " +
			"`name=` sets the handler id (default: the function name); `kind=` " +
			"pins the wire kind (default: the message type's module path).\n\n" +
			"```go\n//fabrik:job\nfunc SendWelcome(ctx context.Context, m WelcomeEmail) error { ... }\n```",
		Example: "//fabrik:job",
		Attrs: []gen.AttrSpec{
			{Key: "name", Kind: gen.KindFreeform},
			{Key: "kind", Kind: gen.KindFreeform},
		},
		Tier: gen.TierBind,
	}
}

func (j *Jobs) Parse(a gen.Annotation) (any, diag.Diagnostics) {
	args, ds := gen.ParseArgs(a, j.Meta())
	nd := &jobNode{pos: a.Pos}
	if v, ok := args.Attr["name"]; ok {
		nd.name = v.Text
	}
	if v, ok := args.Attr["kind"]; ok {
		nd.kind = v.Text
	}
	if ds.HasFatal() {
		return nil, ds
	}
	return nd, ds
}

func (j *Jobs) Check(n any, t gen.Typed) diag.Diagnostics {
	nd := n.(*jobNode)
	fn, sig, ds := funcSig(nd.pos, t, "//fabrik:job")
	if ds.HasFatal() {
		return ds
	}
	p := sig.Params()
	if p.Len() < 2 || !isCtx(p.At(0).Type()) || !isError(sig.Results()) || sig.Results().Len() != 1 {
		ds.Error(nd.pos, fmt.Sprintf("handler %s has the wrong signature", fn.Name()),
			"want func(ctx context.Context, deps..., msg T) error")
		return ds
	}
	nd.msgType = types.Unalias(p.At(p.Len() - 1).Type())
	if _, ok := nd.msgType.(*types.Pointer); ok {
		ds.Error(nd.pos, fmt.Sprintf("handler %s: the message parameter must be a struct value, not a pointer", fn.Name()),
			"messages are plain JSON structs; take T, not *T")
		return ds
	}
	if _, ok := nd.msgType.Underlying().(*types.Struct); !ok {
		ds.Error(nd.pos, fmt.Sprintf("handler %s: the message parameter must be a struct", fn.Name()),
			"messages are plain JSON structs; a scalar, slice, or map is not a message type")
		return ds
	}
	for i := 1; i < p.Len()-1; i++ {
		nd.depTypes = append(nd.depTypes, p.At(i).Type())
	}
	if nd.name == "" {
		nd.name = fn.Name()
	}
	if err := validIdent("name", nd.name); err != nil {
		ds.Error(nd.pos, err.Error(), "names are [A-Za-z0-9._:/-]")
		return ds
	}
	if nd.kind != "" {
		if err := validIdent("kind", nd.kind); err != nil {
			ds.Error(nd.pos, err.Error(), "kinds are [A-Za-z0-9._:/-]")
			return ds
		}
		if strings.HasPrefix(nd.kind, "cron:") {
			ds.Error(nd.pos, fmt.Sprintf("kind %q uses the reserved cron: prefix", nd.kind), "choose a different kind=")
			return ds
		}
	}
	nd.fn = fn.Name()
	nd.pkg = fn.Pkg()
	nd.fset = t.Fset
	j.b.jobs = append(j.b.jobs, nd)
	return ds
}

func (j *Jobs) Emit(n any, g *gen.Gen) diag.Diagnostics {
	if n.(*jobNode).fn != "" {
		j.b.ensure(g)
	}
	return nil
}

func (j *Jobs) Validate(g *gen.Gen) diag.Diagnostics {
	var ds diag.Diagnostics
	if len(j.b.jobs) == 0 && len(j.b.crons) == 0 {
		return ds
	}
	// Cron names are static schedule identities.
	cronSeen := map[string]token.Position{}
	for _, cn := range j.b.crons {
		if cn.name == "" {
			continue
		}
		if first, dup := cronSeen[cn.name]; dup {
			ds.Error(cn.pos, fmt.Sprintf("duplicate //fabrik:cron name %q (also at %s)", cn.name, first), "")
			continue
		}
		cronSeen[cn.name] = cn.pos
	}
	if !j.b.built {
		ds.Warn(j.b.pos(), "jobs are declared but nothing injects *jobs.Manager, so no handler can run",
			"start a worker from a command that injects *jobs.Manager: jobs.NewWorker(mgr, ...).Start(ctx)")
	}
	// The directives own *jobs.Manager; a provider for the same type would
	// be shadowed by the path binding.
	if mgr, ok := g.LookupType(jobsPath, "Manager"); ok && g.HasProviderBinding(types.NewPointer(mgr), "") {
		ds.Error(j.b.pos(), "an app provider returns *jobs.Manager, but //fabrik:job and //fabrik:cron already build and own the manager",
			"remove that provider; the store and jobs.Config providers still apply")
	}
	return ds
}

func (j *Jobs) MissingHint(ty types.Type) (string, bool) {
	switch types.TypeString(types.Unalias(ty), nil) {
	case storePath:
		return "provide a store: //fabrik:provider\nfunc NewJobStore() (jobs.Store, error) { return jobs.NewMemoryStore(), nil }", true
	case managerPath:
		return "declare a job handler: //fabrik:job on func(ctx, deps..., msg T) error", true
	}
	return "", false
}

// Cron implements //fabrik:cron.
type Cron struct{ b *builder }

func (*Cron) Name() string { return "cron" }

func (*Cron) Meta() gen.Meta {
	return gen.Meta{
		Synopsis: "Scheduled function: name=NAME schedule=\"CRON\"",
		Doc: "**`//fabrik:cron name=NAME schedule=\"CRON\"`**\n\n" +
			"Declared on a package function `func(ctx, deps...) error`: it runs " +
			"on the cron schedule, durably, on the jobs worker - no message. The " +
			"first parameter is a `context.Context` (or `jobs.Context`), the rest " +
			"are injected dependencies. `schedule=` is a five-field cron " +
			"expression.\n\n" +
			"```go\n//fabrik:cron name=purge-sessions schedule=\"0 */6 * * *\"\nfunc PurgeSessions(ctx context.Context, s Store) error { ... }\n```",
		Example: "//fabrik:cron name=nightly schedule=\"0 3 * * *\"",
		Attrs: []gen.AttrSpec{
			{Key: "name", Kind: gen.KindFreeform, Required: true},
			{Key: "schedule", Kind: gen.KindFreeform, Required: true},
		},
		Tier: gen.TierBind,
	}
}

func (c *Cron) Parse(a gen.Annotation) (any, diag.Diagnostics) {
	args, ds := gen.ParseArgs(a, c.Meta())
	nd := &cronNode{pos: a.Pos}
	if v, ok := args.Attr["name"]; ok {
		nd.name = v.Text
	}
	if v, ok := args.Attr["schedule"]; ok {
		nd.schedule = v.Text
	}
	if ds.HasFatal() {
		return nil, ds
	}
	return nd, ds
}

func (c *Cron) Check(n any, t gen.Typed) diag.Diagnostics {
	nd := n.(*cronNode)
	fn, sig, ds := funcSig(nd.pos, t, "//fabrik:cron")
	if ds.HasFatal() {
		return ds
	}
	p := sig.Params()
	if p.Len() < 1 || !isCtx(p.At(0).Type()) || !isError(sig.Results()) || sig.Results().Len() != 1 {
		ds.Error(nd.pos, fmt.Sprintf("cron %s has the wrong signature", fn.Name()),
			"want func(ctx context.Context, deps...) error")
		return ds
	}
	for i := 1; i < p.Len(); i++ {
		nd.depTypes = append(nd.depTypes, p.At(i).Type())
	}
	if err := validIdent("name", nd.name); err != nil {
		ds.Error(nd.pos, err.Error(), "give the cron a name: //fabrik:cron name=nightly schedule=\"0 3 * * *\"")
		return ds
	}
	if nd.schedule == "" {
		ds.Error(nd.pos, "//fabrik:cron requires schedule=", "example: schedule=\"0 3 * * *\"")
		return ds
	}
	if _, err := cron.ParseStandard(nd.schedule); err != nil {
		ds.Error(nd.pos, fmt.Sprintf("invalid cron schedule %q: %v", nd.schedule, err),
			"use a five-field cron expression: schedule=\"0 3 * * *\"")
		return ds
	}
	nd.fn = fn.Name()
	nd.pkg = fn.Pkg()
	nd.fset = t.Fset
	c.b.crons = append(c.b.crons, nd)
	return ds
}

func (c *Cron) Emit(n any, g *gen.Gen) diag.Diagnostics {
	if n.(*cronNode).fn != "" {
		c.b.ensure(g)
	}
	return nil
}

func (b *builder) ensure(g *gen.Gen) {
	if b.registered {
		return
	}
	b.registered = true
	g.BindLazyPath(managerPath, func() (string, diag.Diagnostics) { return b.build(g) })
}

func (b *builder) build(g *gen.Gen) (string, diag.Diagnostics) {
	var ds diag.Diagnostics

	storeType, ok := g.LookupType(jobsPath, "Store")
	if !ok {
		ds.Error(b.pos(), "the jobs package is not imported by this app",
			"add a store provider: //fabrik:provider func NewJobStore() (jobs.Store, error) { return jobs.NewMemoryStore(), nil }")
		return "", ds
	}
	storeExpr, sds, ok := g.Instance(storeType, "")
	ds = append(ds, sds...)
	if !ok {
		ds.Error(b.pos(), "no provider for jobs.Store",
			"add a //fabrik:provider returning jobs.Store (jobs.NewMemoryStore() or jobs.NewSQLiteStore(db, ...))")
		return "", ds
	}

	jobsImport := g.Import(jobsPath)
	cfgExpr := jobsImport + ".Config{}"
	if cfgType, ok := g.LookupType(jobsPath, "Config"); ok {
		if e, cds, ok := g.Instance(cfgType, ""); ok {
			ds = append(ds, cds...)
			cfgExpr = e
		}
	}

	kinds := b.resolveKinds(g, &ds)
	if ds.HasFatal() {
		return "", ds
	}

	mgr := g.Var("jobsManager")

	// The manager is a provider; registrations are behavior.
	g.Node(&gen.Raw{
		Base: gen.Base{Phase: gen.PhaseWire, Origin: gen.Origin{Pos: b.pos()}},
		Lines: []string{
			fmt.Sprintf("%s, err := %s.New(%s, %s)", mgr, jobsImport, storeExpr, cfgExpr),
			"if err != nil {", "return err", "}",
		},
		Defines: []string{mgr},
	})

	var lines []string
	for _, k := range kinds {
		lines = append(lines,
			fmt.Sprintf("if err := %s.Register[%s](%s, %q); err != nil {", jobsImport, g.TypeExpr(k.msgType), mgr, k.kind),
			"return err", "}")
	}
	for _, jn := range b.jobs {
		deps, dds := resolveDeps(g, jn.pos, jn.depTypes)
		ds = append(ds, dds...)
		call := callExpr(g, jn.pkg, jn.fn, deps, true)
		lines = append(lines,
			fmt.Sprintf("if err := %s.On[%s](%s, %q, func(c %s.Context, m %s) error {", jobsImport, g.TypeExpr(jn.msgType), mgr, jn.name, jobsImport, g.TypeExpr(jn.msgType)),
			"return "+call,
			"}); err != nil {",
			"return err", "}")
	}
	for _, cn := range b.crons {
		deps, dds := resolveDeps(g, cn.pos, cn.depTypes)
		ds = append(ds, dds...)
		call := callExpr(g, cn.pkg, cn.fn, deps, false)
		lines = append(lines,
			fmt.Sprintf("if err := %s.RegisterCron(%s, %q, %q, func(c %s.Context) error {", jobsImport, mgr, cn.name, cn.schedule, jobsImport),
			"return "+call,
			"}); err != nil {",
			"return err", "}")
	}
	// Schedule sync is a start-time concern, after the schema exists.

	if len(lines) > 0 {
		g.Node(&gen.Raw{
			Base:  gen.Base{Phase: gen.PhaseRegister, Label: "Jobs", Origin: gen.Origin{Pos: b.pos()}},
			Lines: lines,
		})
	}

	b.built = true
	return mgr, ds
}

// callExpr builds `pkg.Fn(c, deps..., [m])`.
func callExpr(g *gen.Gen, pkg *types.Package, fn string, deps []string, withMsg bool) string {
	args := append([]string{"c"}, deps...)
	if withMsg {
		args = append(args, "m")
	}
	return g.ImportPkg(pkg) + "." + fn + "(" + strings.Join(args, ", ") + ")"
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

type kindDecl struct {
	kind    string
	msgType types.Type
}

func (b *builder) resolveKinds(g *gen.Gen, ds *diag.Diagnostics) []kindDecl {
	byType := map[string]*kindDecl{}
	var order []string
	kindOwner := map[string]string{}
	handlerSeen := map[string]token.Position{}

	for _, d := range b.jobs {
		key := types.TypeString(d.msgType, nil)
		kd, ok := byType[key]
		if !ok {
			kind, err := b.kindFor(g, d)
			if err != "" {
				ds.Error(d.pos, err, "pin it with kind=NAME")
				continue
			}
			if owner, dup := kindOwner[kind]; dup && owner != key {
				ds.Error(d.pos, fmt.Sprintf("kind %q is already used by a different message type", kind), "give one a distinct kind=")
				continue
			}
			kindOwner[kind] = key
			kd = &kindDecl{kind: kind, msgType: d.msgType}
			byType[key] = kd
			order = append(order, key)
		} else if d.kind != "" && d.kind != kd.kind {
			ds.Error(d.pos, fmt.Sprintf("conflicting kind= for one message type: %q vs %q", d.kind, kd.kind), "")
			continue
		}
		d.kind = kd.kind
		hk := kd.kind + "\x00" + d.name
		if first, dup := handlerSeen[hk]; dup {
			ds.Error(d.pos, fmt.Sprintf("duplicate handler name %q for kind %q (also at %s)", d.name, kd.kind, first), "")
			continue
		}
		handlerSeen[hk] = d.pos
	}

	out := make([]kindDecl, 0, len(order))
	for _, key := range order {
		out = append(out, *byType[key])
	}
	sort.Slice(out, func(a, c int) bool { return out[a].kind < out[c].kind })
	return out
}

func (b *builder) kindFor(g *gen.Gen, d *jobNode) (kind, errMsg string) {
	if d.kind != "" {
		return d.kind, ""
	}
	named, ok := d.msgType.(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return "", "cannot derive a kind from an unnamed message type"
	}
	pkgPath := named.Obj().Pkg().Path()
	mod := g.Module()
	if pkgPath == mod {
		return "", "cannot derive a kind for a message at the module root"
	}
	rel := strings.TrimPrefix(pkgPath, mod+"/")
	if rel == pkgPath {
		return "", fmt.Sprintf("cannot derive a kind for message type %s declared outside this module", named.Obj().Name())
	}
	return rel + "." + named.Obj().Name(), ""
}

func funcSig(pos token.Position, t gen.Typed, directive string) (*types.Func, *types.Signature, diag.Diagnostics) {
	var ds diag.Diagnostics
	fn, ok := t.Target.(*types.Func)
	if !ok {
		ds.Error(pos, directive+" must be on a function", "")
		return nil, nil, ds
	}
	sig := fn.Signature()
	if sig.TypeParams().Len() > 0 || sig.RecvTypeParams().Len() > 0 {
		ds.Error(pos, fmt.Sprintf("%s handler %s cannot be generic", directive, fn.Name()), "declare a concrete function")
		return nil, nil, ds
	}
	if sig.Recv() != nil {
		ds.Error(pos, directive+" must be a package function, not a method",
			"dependencies are parameters, not receiver fields")
		return nil, nil, ds
	}
	return fn, sig, ds
}

func isCtx(t types.Type) bool {
	switch typePath(t) {
	case "context.Context", jobsContextPath:
		return true
	}
	return false
}

func isError(res *types.Tuple) bool {
	return res.Len() == 1 && typePath(res.At(0).Type()) == "error"
}

func typePath(t types.Type) string {
	t = types.Unalias(t)
	if p, ok := t.(*types.Pointer); ok {
		return "*" + typePath(p.Elem())
	}
	return types.TypeString(t, nil)
}

func validIdent(what, s string) error {
	if s == "" {
		return fmt.Errorf("%s is empty", what)
	}
	if len(s) > 255 {
		return fmt.Errorf("%s %q exceeds 255 bytes", what, s)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == ':' || c == '/' || c == '-':
		default:
			return fmt.Errorf("%s %q has invalid byte %q", what, s, string(c))
		}
	}
	return nil
}
