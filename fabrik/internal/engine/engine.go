// Package engine runs directive loading, validation, emission, and rendering.
package engine

import (
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/fabrik/internal/load"
	"github.com/gofabrik/fabrik/gen"
)

// Result contains generated source and diagnostics; Src is nil after a fatal diagnostic.
type Result struct {
	Src     []byte
	MainDir string
	Diags   diag.Diagnostics
}

// Wire generates main.gen.go for the module rooted at dir, applying overlays in place of on-disk files.
func Wire(dir string, overlay map[string][]byte) (*Result, error) {
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
		return &Result{MainDir: res.MainDir, Diags: res.Diags}, nil
	}

	directives := New()
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
		return &Result{MainDir: res.MainDir, Diags: diags}, nil
	}

	g := gen.New()
	g.SetModule(res.ModulePath)
	g.SetTypes(res.Types)
	for _, d := range directives {
		if h, ok := d.(gen.Hinter); ok {
			g.AddMissingHint(h.MissingHint)
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
	// Validate all lazy bindings before materializing command scopes.
	if g.ScopeCount() > 0 {
		diags = append(diags, g.RunValidationPass()...)
		if !diags.HasFatal() {
			diags = append(diags, g.MaterializeScopes()...)
		}
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
	diags.Sort()
	if diags.HasFatal() {
		return &Result{MainDir: res.MainDir, Diags: diags}, nil
	}

	src, err := g.Render()
	if err != nil {
		return nil, err
	}
	return &Result{Src: src, MainDir: res.MainDir, Diags: diags}, nil
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
