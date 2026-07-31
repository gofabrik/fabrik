// Package engine runs directive loading, validation, emission, and rendering.
package engine

import (
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/fabrik/internal/genconfig"
	"github.com/gofabrik/fabrik/fabrik/internal/load"
	"github.com/gofabrik/fabrik/gen"
)

// Result contains generated source and diagnostics; Src is nil after a fatal diagnostic.
type Result struct {
	// Src is the single-file output; nil under split, where Files
	// carries the set. Files is populated in both modes.
	Src     []byte
	Files   map[string][]byte
	MainDir string
	OutDir  string // directory the generated output belongs to; equals MainDir until split/embedded modes relocate it
	Diags   diag.Diagnostics
	Graph   *gen.Graph // Non-nil only when Options.Graph is true.
}

// Options configures generation beyond the defaults.
type Options struct {
	Comments gen.CommentLevel
	Graph    bool
	BuildTag string // constrains the generated file with a //go:build line
	Split    bool   // one file per extracted region instead of a single main.gen.go
}

// OptionsFrom is the one mapping from the resolved project
// configuration to engine options; every surface uses it so none
// duplicates the field correspondence.
func OptionsFrom(cfg genconfig.Options) Options {
	return Options{Comments: cfg.Comments, BuildTag: cfg.BuildTag, Split: cfg.Split == genconfig.SplitFragment}
}

// Wire generates main.gen.go for the module rooted at dir with default
// options, applying overlays in place of on-disk files.
func Wire(dir string, overlay map[string][]byte) (*Result, error) {
	return WireOptions(dir, overlay, Options{})
}

// WireOptions is Wire with explicit generation options.
func WireOptions(dir string, overlay map[string][]byte, opts Options) (*Result, error) {
	res, err := load.Load(dir, overlay)
	if err != nil {
		return nil, err
	}
	if res.MainDir == "" {
		// Report directive syntax even when no main package is found.
		anns := make([]gen.Annotation, len(res.Items))
		for i, item := range res.Items {
			anns[i] = item.Ann
		}
		res.Diags = append(res.Diags, SyntaxDiags(anns)...)
		res.Diags.Sort()
		return &Result{Diags: res.Diags}, fmt.Errorf("no package main found under %s", res.Root)
	}
	// Stop before directive checks when package loading is invalid.
	if res.Diags.HasFatal() {
		res.Diags.Sort()
		return &Result{MainDir: res.MainDir, OutDir: res.MainDir, Diags: res.Diags}, nil
	}

	directives := New()
	for _, d := range directives {
		if m, ok := d.(interface{ SetModuleRoot(string) }); ok {
			m.SetModuleRoot(res.Root)
		}
	}
	if len(overlay) > 0 {
		// Non-Go validation must use the same overlays as package loading.
		for _, d := range directives {
			if t, ok := d.(interface{ SetTreeFS(func(string) fs.FS) }); ok {
				t.SetTreeFS(func(dir string) fs.FS { return overlayDirFS{dir: dir, overlay: overlay} })
			}
		}
	}
	byName, names := registryIndex(directives)

	diags := res.Diags
	var parsed []gen.Parsed
	for _, item := range res.Items {
		d, ok := byName[item.Ann.Name]
		if !ok {
			diags = append(diags, unknownDirectiveDiag(item.Ann, names))
			continue
		}
		var node any
		var ds diag.Diagnostics
		if err := guard(d.Name(), "Parse", func() { node, ds = d.Parse(item.Ann) }); err != nil {
			return nil, err
		}
		diags = append(diags, ds...)
		if node == nil || ds.HasFatal() {
			continue
		}
		if err := guard(d.Name(), "Check", func() { ds = d.Check(node, item.Typed) }); err != nil {
			return nil, err
		}
		diags = append(diags, ds...)
		if ds.HasFatal() {
			continue
		}
		parsed = append(parsed, gen.Parsed{Directive: d, Node: node})
	}

	// Emit only after project-wide Parse and Check pass.
	if diags.HasFatal() {
		diags.Sort()
		return &Result{MainDir: res.MainDir, OutDir: res.MainDir, Diags: diags}, nil
	}

	g := gen.New()
	g.FragmentMode()
	g.SetModule(res.ModulePath)
	g.SetTypes(res.Types)
	g.ReserveOutputIdents(res.MainIdents)
	g.SetCommentLevel(opts.Comments)
	g.SetBuildTag(opts.BuildTag)
	g.SetSourceRoot(res.Root)
	for _, d := range directives {
		if h, ok := d.(gen.Hinter); ok {
			g.AddMissingHint(h.MissingHint)
		}
	}
	// Directives within a tier may emit in any order.
	for _, d := range directives {
		if src, ok := d.(gen.InjectSource); ok {
			g.SeedInjectNames(src.InjectMappings())
		}
	}
	emitTierNodes := func(tier gen.EmitTier) error {
		for _, p := range parsed {
			if p.Directive.Meta().Tier != tier {
				continue
			}
			g.SetDirective(p.Directive.Name())
			if err := guard(p.Directive.Name(), "Emit", func() {
				diags = append(diags, p.Directive.Emit(p.Node, g)...)
			}); err != nil {
				return err
			}
		}
		return nil
	}
	if err := emitTierNodes(gen.TierBind); err != nil {
		return nil, err
	}
	// Prepare bindings before dependency resolution.
	for _, p := range parsed {
		np, ok := p.Directive.(gen.NodePreparer)
		if !ok {
			continue
		}
		g.SetDirective(p.Directive.Name())
		if err := guard(p.Directive.Name(), "PrepareNode", func() { np.PrepareNode(p.Node, g) }); err != nil {
			return nil, err
		}
	}
	for _, tier := range []gen.EmitTier{gen.TierHook, gen.TierMain} {
		if err := emitTierNodes(tier); err != nil {
			return nil, err
		}
	}
	// Validators run after finishers because finishers may emit.
	for _, d := range directives {
		f, ok := d.(gen.Finisher)
		if !ok {
			continue
		}
		g.SetDirective(d.Name())
		if err := guard(d.Name(), "Finish", func() {
			diags = append(diags, f.Finish(g)...)
		}); err != nil {
			return nil, err
		}
	}
	diags = append(diags, g.BindConflicts()...)
	// Flows walk first so demand usage exists; the validation pass then
	// sweeps only the bindings no flow reached. Prior fatal diagnostics
	// skip the walk, leaving the sweep complete.
	if g.ScopeCount() > 0 {
		if !diags.HasFatal() {
			scopeDiags, err := guardedScopePass("materialization", g.WalkFlows)
			if err != nil {
				return nil, err
			}
			diags = append(diags, scopeDiags...)
			diags = append(diags, g.BindConflicts()...)
		}
		scopeDiags, err := guardedScopePass("validation", g.RunValidationPass)
		if err != nil {
			return nil, err
		}
		diags = append(diags, scopeDiags...)
		diags = append(diags, g.BindConflicts()...)
	}
	for _, d := range directives {
		v, ok := d.(gen.Validator)
		if !ok {
			continue
		}
		g.SetDirective(d.Name())
		if err := guard(d.Name(), "Validate", func() {
			diags = append(diags, v.Validate(g)...)
		}); err != nil {
			return nil, err
		}
	}
	// Scope passes register binds too; drain conflicts they recorded.
	diags = append(diags, g.BindConflicts()...)
	if !diags.HasFatal() {
		diags = append(diags, g.PlanFragments()...)
	}
	diags = append(diags, g.ValidateGraph()...)
	diags.Sort()
	if diags.HasFatal() {
		return &Result{MainDir: res.MainDir, OutDir: res.MainDir, Diags: diags}, nil
	}

	out := &Result{MainDir: res.MainDir, OutDir: res.MainDir, Diags: diags}
	if opts.Split {
		files, err := g.RenderFiles()
		if err != nil {
			return nil, err
		}
		out.Files = files
	} else {
		src, err := g.Render()
		if err != nil {
			return nil, err
		}
		out.Src = src
		out.Files = map[string][]byte{"main.gen.go": src}
	}
	if opts.Graph {
		// Graph uses import aliases finalized by Render.
		out.Graph = g.Graph()
	}
	return out, nil
}

func registryIndex(directives []gen.Directive) (map[string]gen.Directive, []string) {
	byName := map[string]gen.Directive{}
	names := make([]string, 0, len(directives))
	for _, d := range directives {
		if d.Meta().Hidden {
			// Hidden finishers are not user directives, so their names remain unknown.
			continue
		}
		byName[d.Name()] = d
		names = append(names, d.Name())
	}
	sort.Strings(names)
	return byName, names
}

func unknownDirectiveDiag(ann gen.Annotation, names []string) diag.Diagnostic {
	if ann.Name == "" {
		return diag.Diagnostic{
			Severity: diag.SevError, Pos: ann.Pos,
			Message: `empty directive after "fabrik:"`,
			Help:    "expected one of: " + strings.Join(names, ", "),
		}
	}
	return diag.Diagnostic{
		Severity: diag.SevError, Pos: ann.Pos,
		Message: fmt.Sprintf("unknown directive %q", "fabrik:"+ann.Name),
		Help:    "known: " + strings.Join(names, ", "),
	}
}

// SyntaxDiags parses annotations without type information.
func SyntaxDiags(anns []gen.Annotation) diag.Diagnostics {
	byName, names := registryIndex(New())
	var ds diag.Diagnostics
	for _, ann := range anns {
		d, ok := byName[ann.Name]
		if !ok {
			ds = append(ds, unknownDirectiveDiag(ann, names))
			continue
		}
		_, pds := d.Parse(ann)
		ds = append(ds, pds...)
	}
	return ds
}

// guard attributes directive panics to the failing directive and phase.
func guard(name, phase string, fn func()) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("internal error: directive %q panicked during %s: %v", name, phase, p)
		}
	}()
	fn()
	return nil
}

func guardedScopePass(phase string, fn func() diag.Diagnostics) (ds diag.Diagnostics, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("internal error: command scope %s panicked: %v", phase, p)
		}
	}()
	return fn(), nil
}
