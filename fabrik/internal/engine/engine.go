// Package engine runs directive loading, validation, emission, and rendering.
package engine

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/fabrik/internal/genconfig"
	"github.com/gofabrik/fabrik/fabrik/internal/load"
	"github.com/gofabrik/fabrik/gen"
)

// Result contains generated source and diagnostics; Src is nil after a fatal diagnostic.
type Result struct {
	// Src is nil for split output; Files is populated in both modes.
	Src     []byte
	Files   map[string][]byte
	MainDir string
	OutDir  string // generated output directory
	Diags   diag.Diagnostics
	Graph   *gen.Graph // Non-nil only when Options.Graph is true.
	// Stale lists generated files outside the configured output
	// directory, left behind by an earlier emit or dir setting.
	Stale []string
}

// Options configures generation beyond the defaults.
type Options struct {
	Comments gen.CommentLevel
	Graph    bool
	BuildTag string // generated file build constraint
	Split    bool   // emit extracted regions as separate files

	Embedded      bool
	Dir           string // output directory relative to the module root
	Package       string
	Entrypoints   []string
	EmitPos       token.Position
	DirPos        token.Position
	EntrypointPos map[string]token.Position
}

// OptionsFrom converts resolved project configuration to engine options.
func OptionsFrom(cfg genconfig.Options) Options {
	return Options{
		Comments:      cfg.Comments,
		BuildTag:      cfg.BuildTag,
		Split:         cfg.Split == genconfig.SplitFragment,
		Embedded:      cfg.Emit == genconfig.EmitEmbedded,
		Dir:           cfg.Dir,
		Package:       cfg.Package,
		Entrypoints:   cfg.Entrypoints,
		EmitPos:       cfg.EmitPos,
		DirPos:        cfg.DirPos,
		EntrypointPos: cfg.EntrypointPos,
	}
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
	if res.MainDir == "" && !opts.Embedded {
		// Report directive syntax even when no main package is found.
		anns := make([]gen.Annotation, len(res.Items))
		for i, item := range res.Items {
			anns[i] = item.Ann
		}
		res.Diags = append(res.Diags, SyntaxDiags(anns)...)
		res.Diags.Sort()
		return &Result{Diags: res.Diags}, fmt.Errorf("no package main found under %s", res.Root)
	}
	outDir := res.MainDir
	if opts.Embedded {
		outDir = filepath.Join(res.Root, filepath.FromSlash(opts.Dir))
		resolved, contained, cerr := resolveDir(res.Root, outDir)
		if cerr != nil {
			return nil, cerr
		}
		if !contained {
			res.Diags.Error(opts.DirPos, fmt.Sprintf("dir %q resolves outside the module", opts.Dir),
				"a symlink in the path escapes the module root")
		} else {
			// The physical path keys every directory-indexed lookup:
			// stale detection, package names, and identifier reservation.
			outDir = resolved
		}
	}
	stale := staleGenerated(res.GeneratedFiles, outDir)
	// Stop before directive checks when package loading is invalid.
	if res.Diags.HasFatal() {
		res.Diags.Sort()
		return &Result{MainDir: res.MainDir, OutDir: outDir, Diags: res.Diags, Stale: stale}, nil
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
		return &Result{MainDir: res.MainDir, OutDir: outDir, Diags: diags, Stale: stale}, nil
	}

	g := gen.New()
	g.FragmentMode()
	g.SetModule(res.ModulePath)
	g.SetTypes(res.Types)
	if opts.Embedded {
		g.ReserveOutputIdents(res.PackageIdents[outDir])
	} else {
		g.ReserveOutputIdents(res.MainIdents)
	}
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
	if opts.Embedded {
		if g.CommandCount() == 0 {
			diags = append(diags, diag.Diagnostic{
				Severity: diag.SevError, Pos: opts.EmitPos,
				Message: "embedded output exports command entry points; declare //fabrik:cli:command functions",
			})
		} else {
			diags = append(diags, g.SelectCommands(opts.Entrypoints, opts.EntrypointPos)...)
		}
		if opts.Package == "main" {
			diags = append(diags, diag.Diagnostic{
				Severity: diag.SevError, Pos: opts.EmitPos,
				Message: "embedded output is a non-main package",
				Help:    "set generate.package (or dir) to a library package name",
			})
		} else if existing, ok := res.PackageNames[outDir]; ok && existing != opts.Package {
			diags = append(diags, diag.Diagnostic{
				Severity: diag.SevError, Pos: opts.EmitPos,
				Message: fmt.Sprintf("generate.package %q differs from package %q already declared in %s", opts.Package, existing, opts.Dir),
			})
		}
		g.EmbeddedOutput(opts.Package)
	}
	// Validation sweeps only bindings not reached by the preceding flow walk.
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
	// Include conflicts registered during scope passes.
	diags = append(diags, g.BindConflicts()...)
	if !diags.HasFatal() {
		diags = append(diags, g.PlanFragments()...)
	}
	diags = append(diags, g.ValidateGraph()...)
	diags.Sort()
	if diags.HasFatal() {
		return &Result{MainDir: res.MainDir, OutDir: outDir, Diags: diags, Stale: stale}, nil
	}

	out := &Result{MainDir: res.MainDir, OutDir: outDir, Diags: diags, Stale: stale}
	mainName := "main.gen.go"
	if opts.Embedded {
		mainName = "fabrik.gen.go"
	}
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
		out.Files = map[string][]byte{mainName: src}
	}
	if opts.Embedded {
		// Cycle detection uses only imports retained in the rendered files.
		// Use the resolved directory to match the loader's physical-path index.
		outPkgPath := res.ModulePath
		if rel, err := filepath.Rel(res.Root, outDir); err == nil && rel != "." {
			outPkgPath += "/" + filepath.ToSlash(rel)
		}
		rendered, err := renderedImports(out.Files)
		if err != nil {
			return nil, err
		}
		if cyclic := importsTransitively(res.Imports, rendered, outPkgPath); cyclic != "" {
			diags = append(diags, diag.Diagnostic{
				Severity: diag.SevError, Pos: opts.EmitPos,
				Message: fmt.Sprintf("generated code imports %s, which imports the output package %s", cyclic, outPkgPath),
				Help:    "point generate.dir at a package the injected packages do not depend on",
			})
			diags.Sort()
			return &Result{MainDir: res.MainDir, OutDir: outDir, Diags: diags, Stale: stale}, nil
		}
	}
	if opts.Graph {
		// Graph uses import aliases finalized by Render.
		out.Graph = g.Graph()
	}
	return out, nil
}

// renderedImports parses each generated file's import block.
func renderedImports(files map[string][]byte) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for name, src := range files {
		f, err := parser.ParseFile(token.NewFileSet(), name, src, parser.ImportsOnly)
		if err != nil {
			return nil, err
		}
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			if !seen[path] {
				seen[path] = true
				out = append(out, path)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// importsTransitively reports the first generated import that reaches
// target through the module's import graph.
func importsTransitively(graph map[string][]string, from []string, target string) string {
	for _, start := range from {
		if start == target {
			return start
		}
		seen := map[string]bool{start: true}
		queue := []string{start}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			for _, next := range graph[cur] {
				if next == target {
					return start
				}
				if !seen[next] {
					seen[next] = true
					queue = append(queue, next)
				}
			}
		}
	}
	return ""
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

// resolveDir resolves symlinks in target's nearest existing ancestor,
// rejoins the not-yet-created remainder, and reports whether the result
// stays inside root.
func resolveDir(root, target string) (string, bool, error) {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", false, err
	}
	var tail []string
	for p := target; ; p = filepath.Dir(p) {
		resolved, err := filepath.EvalSymlinks(p)
		if err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			contained := resolved == resolvedRoot ||
				strings.HasPrefix(resolved, resolvedRoot+string(filepath.Separator))
			return resolved, contained, nil
		}
		if !os.IsNotExist(err) {
			return "", false, err
		}
		if filepath.Dir(p) == p {
			return target, true, nil
		}
		tail = append(tail, filepath.Base(p))
	}
}

// staleGenerated lists generated files that live outside the configured
// output directory: leftovers from an earlier emit or dir setting.
func staleGenerated(generated []string, outDir string) []string {
	var stale []string
	for _, f := range generated {
		if filepath.Dir(f) != outDir {
			stale = append(stale, f)
		}
	}
	return stale
}
