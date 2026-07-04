// Package codegen generates main.gen.go from the model produced by scan.
package codegen

import (
	"bytes"
	_ "embed"
	"fmt"
	"go/format"
	"sort"
	"strings"
	"text/template"

	"github.com/gofabrik/fabrik/fabrik/internal/diag"
	"github.com/gofabrik/fabrik/fabrik/internal/scan"
)

// Generate produces the formatted source of main.gen.go for the project. It also returns
// diagnostics (duplicate providers, unresolved dependencies, directives in
// package main). When the diagnostics contain a fatal error the returned source
// is nil. A non-nil error indicates a structural problem (e.g. no package main).
func Generate(project *scan.Project) ([]byte, diag.Diagnostics, error) {
	if project.MainDir == "" {
		return nil, nil, fmt.Errorf("no package main found under %s", project.Root)
	}

	g := &generator{project: project, providers: map[string]string{}}
	rc := g.build()
	if g.diags.HasFatal() {
		return nil, g.diags, nil
	}

	var buf bytes.Buffer
	if err := wireTemplate.Execute(&buf, rc); err != nil {
		return nil, g.diags, fmt.Errorf("render template: %w", err)
	}
	src, err := format.Source(buf.Bytes())
	if err != nil {
		return nil, g.diags, fmt.Errorf("format generated source: %w\n%s", err, buf.String())
	}
	return src, g.diags, nil
}

type generator struct {
	project   *scan.Project
	diags     diag.Diagnostics
	providers map[string]string // canonical type -> generated var name
}

type renderContext struct {
	UseContext    bool
	ModuleImports []string
	ProviderDecls []providerDecl
	Routes        []routeRender
}

type providerDecl struct {
	Kind    string // "single" or "struct"
	VarName string

	// single
	PkgName string
	Func    string
	Args    string

	// struct
	StructType string
	Fields     []structField
}

type structField struct {
	Name string
	Expr string
}

type routeRender struct {
	Method      string
	Path        string
	HandlerExpr string
}

func (g *generator) build() *renderContext {
	rc := &renderContext{}

	// Reject duplicate package names: vars and imports are keyed on the short
	// package name, so two same-named packages would collide.
	byName := map[string][]*scan.Package{}
	for _, pkg := range g.project.Packages {
		if pkg.Name == "main" {
			continue
		}
		byName[pkg.Name] = append(byName[pkg.Name], pkg)
	}
	for name, pkgs := range byName {
		if len(pkgs) > 1 {
			for _, pkg := range pkgs {
				g.diags = append(g.diags, diag.Diagnostic{
					Severity: diag.SevError, Pos: pkg.Pos,
					Message: fmt.Sprintf("duplicate package name %q", name),
					Help:    "give each package a unique name",
				})
			}
		}
	}
	if g.diags.HasFatal() {
		return rc // a name collision poisons every downstream lookup
	}

	// Warn about directives in package main: they are not wired.
	for _, pkg := range g.project.Packages {
		if pkg.Name != "main" {
			continue
		}
		for _, r := range pkg.Routes {
			g.diags = append(g.diags, diag.Diagnostic{
				Severity: diag.SevWarning, Pos: r.Pos,
				Message: "//fabrik:http in package main is ignored",
				Help:    "move the handler to a subpackage",
			})
		}
		for _, p := range pkg.Providers {
			g.diags = append(g.diags, diag.Diagnostic{
				Severity: diag.SevWarning, Pos: p.Pos,
				Message: "//fabrik:provider in package main is ignored",
				Help:    "move the provider to a subpackage",
			})
		}
	}

	// Group providers by canonical return type to detect duplicates.
	type provItem struct {
		pkg string
		p   *scan.Provider
	}
	groups := map[string][]provItem{}
	for _, pkg := range g.project.Packages {
		if pkg.Name == "main" {
			continue
		}
		for _, p := range pkg.Providers {
			canon := canonicalType(pkg.Name, p.Returns)
			groups[canon] = append(groups[canon], provItem{pkg.Name, p})
		}
	}
	for canon, grp := range groups {
		if len(grp) > 1 {
			for _, it := range grp {
				g.diags = append(g.diags, diag.Diagnostic{
					Severity: diag.SevError, Pos: it.p.Pos,
					Message: fmt.Sprintf("multiple providers for type %s", canon),
					Help:    "only one //fabrik:provider per type is supported",
				})
			}
			continue
		}
		it := grp[0]
		g.providers[canon] = providerVarName(it.pkg, it.p.Returns)
	}

	// Register receiver struct var names before resolving fields, so a struct
	// field may depend on another wired struct.
	type recvKey struct{ pkg, typ string }
	receivers := map[recvKey]*scan.Route{}
	for _, pkg := range g.project.Packages {
		if pkg.Name == "main" {
			continue
		}
		for _, r := range pkg.Routes {
			if r.Receiver == "" {
				continue
			}
			k := recvKey{pkg.Name, strings.TrimPrefix(r.Receiver, "*")}
			if receivers[k] == nil {
				receivers[k] = r
			}
		}
	}
	for rk := range receivers {
		g.providers["*"+rk.pkg+"."+rk.typ] = providerVarName(rk.pkg, rk.typ)
		g.providers[rk.pkg+"."+rk.typ] = providerVarName(rk.pkg, rk.typ)
	}

	// Provider decls (constructors) in stable canonical-type order.
	var decls []providerDecl
	canons := make([]string, 0, len(groups))
	for c := range groups {
		canons = append(canons, c)
	}
	sort.Strings(canons)
	for _, canon := range canons {
		grp := groups[canon]
		if len(grp) != 1 {
			continue
		}
		it := grp[0]
		decls = append(decls, providerDecl{
			Kind: "single", VarName: g.providers[canon],
			PkgName: it.pkg, Func: it.p.Func, Args: g.resolveArgs(it.pkg, it.p.Params),
		})
	}

	// Receiver struct decls, sorted by var name for determinism.
	recvList := make([]recvKey, 0, len(receivers))
	for rk := range receivers {
		recvList = append(recvList, rk)
	}
	sort.Slice(recvList, func(i, j int) bool {
		return providerVarName(recvList[i].pkg, recvList[i].typ) < providerVarName(recvList[j].pkg, recvList[j].typ)
	})
	for _, rk := range recvList {
		pkg := findPackage(g.project, rk.pkg)
		fields, ok := pkg.Structs[rk.typ]
		if !ok {
			g.diags = append(g.diags, diag.Diagnostic{
				Severity: diag.SevError, Pos: receivers[rk].Pos,
				Message: fmt.Sprintf("handler receiver %s.%s is not a struct", rk.pkg, rk.typ),
				Help:    "//fabrik:http handlers must be methods on a struct",
			})
			continue
		}
		var fdecls []structField
		for _, f := range fields {
			if f.Name == "" {
				continue // embedded field: not wired by field name
			}
			expr, ok := g.providers[canonicalType(rk.pkg, f.Type)]
			if !ok {
				g.diags = append(g.diags, diag.Diagnostic{
					Severity: diag.SevError, Pos: f.Pos,
					Message: fmt.Sprintf("no provider for %s (field %s of %s.%s)", f.Type, f.Name, rk.pkg, rk.typ),
					Help:    fmt.Sprintf("add a //fabrik:provider returning %s", f.Type),
				})
				expr = "nil"
			}
			fdecls = append(fdecls, structField{Name: f.Name, Expr: expr})
		}
		decls = append(decls, providerDecl{
			Kind: "struct", VarName: providerVarName(rk.pkg, rk.typ),
			StructType: rk.pkg + "." + rk.typ, Fields: fdecls,
		})
	}

	rc.ProviderDecls = topoSort(pruneUnused(decls))

	// Routes, rejecting duplicate method+path pairs.
	seenRoutes := map[string]*scan.Route{}
	usedPkgs := map[string]bool{}
	for _, pkg := range g.project.Packages {
		if pkg.Name == "main" {
			continue
		}
		for _, r := range pkg.Routes {
			key := r.Method + " " + r.Path
			if prev, ok := seenRoutes[key]; ok {
				g.diags = append(g.diags, diag.Diagnostic{
					Severity: diag.SevError, Pos: r.Pos,
					Message: fmt.Sprintf("duplicate route %s %s", r.Method, r.Path),
					Help:    fmt.Sprintf("first declared at %s", prev.Pos),
				})
				continue
			}
			seenRoutes[key] = r
			usedPkgs[pkg.Name] = true
			rc.Routes = append(rc.Routes, routeRender{
				Method: r.Method, Path: r.Path,
				HandlerExpr: buildRouteHandler(pkg.Name, r.Func, r.Receiver),
			})
		}
	}

	// The ctx var is emitted only when a surviving provider consumes it.
	for _, d := range rc.ProviderDecls {
		if d.Kind == "single" && containsIdent(d.Args, "ctx") {
			rc.UseContext = true
			break
		}
	}

	// Imports: packages referenced by the emitted decls and routes.
	for _, d := range rc.ProviderDecls {
		switch d.Kind {
		case "single":
			usedPkgs[d.PkgName] = true
		case "struct":
			usedPkgs[pkgName(d.StructType)] = true
		}
	}
	importPath := map[string]string{}
	for _, pkg := range g.project.Packages {
		importPath[pkg.Name] = pkg.ImportPath
	}
	for name := range usedPkgs {
		if p := importPath[name]; p != "" {
			rc.ModuleImports = append(rc.ModuleImports, p)
		}
	}
	sort.Strings(rc.ModuleImports)

	return rc
}

// resolveArgs builds the call-argument string for a provider's parameters.
// context.Context params become "ctx"; other params resolve to wired vars.
// Unresolved params produce a diagnostic anchored at the provider directive.
func (g *generator) resolveArgs(pkg string, params []scan.Param) string {
	var args []string
	for _, p := range params {
		if p.Type == "context.Context" {
			args = append(args, "ctx")
			continue
		}
		v, ok := g.providers[canonicalType(pkg, p.Type)]
		if !ok {
			g.diags = append(g.diags, diag.Diagnostic{
				Severity: diag.SevError, Pos: p.Pos,
				Message: fmt.Sprintf("no provider for %s", p.Type),
				Help:    fmt.Sprintf("add a //fabrik:provider returning %s", p.Type),
			})
			v = "nil"
		}
		args = append(args, v)
	}
	return strings.Join(args, ", ")
}

// canonicalType returns the global key under which a type is registered. Types
// without a package qualifier are qualified with the consumer's package; a
// leading "*" is preserved.
func canonicalType(consumerPkg, typ string) string {
	star := ""
	t := typ
	if strings.HasPrefix(t, "*") {
		star, t = "*", t[1:]
	}
	if strings.Contains(t, ".") {
		return star + t
	}
	return star + consumerPkg + "." + t
}

// providerVarName derives the generated variable name for a value of retType
// produced in pkg: "*sqlite.DB" in pkg "shared" -> "sharedDB".
func providerVarName(pkg, retType string) string {
	rt := strings.TrimPrefix(retType, "*")
	if i := strings.LastIndex(rt, "."); i >= 0 {
		rt = rt[i+1:]
	}
	return pkg + rt
}

func buildRouteHandler(pkg, fn, receiver string) string {
	if receiver != "" {
		return providerVarName(pkg, strings.TrimPrefix(receiver, "*")) + "." + fn
	}
	return pkg + "." + fn
}

func findPackage(project *scan.Project, name string) *scan.Package {
	for _, p := range project.Packages {
		if p.Name == name {
			return p
		}
	}
	return nil
}

// declRefs returns the var names a decl references through its call args or
// struct-field expressions.
func declRefs(d providerDecl, byVar map[string]providerDecl) []string {
	var refs []string
	seen := map[string]bool{}
	add := func(text string) {
		for v := range byVar {
			if v == d.VarName || seen[v] {
				continue
			}
			if containsIdent(text, v) {
				refs = append(refs, v)
				seen[v] = true
			}
		}
	}
	add(d.Args)
	for _, f := range d.Fields {
		add(f.Expr)
	}
	return refs
}

// pruneUnused drops provider decls not reachable from any struct decl. Struct
// decls are the handlers, which the routes always reference.
func pruneUnused(decls []providerDecl) []providerDecl {
	byVar := map[string]providerDecl{}
	for _, d := range decls {
		byVar[d.VarName] = d
	}
	reachable := map[string]bool{}
	var mark func(string)
	mark = func(name string) {
		if reachable[name] {
			return
		}
		d, ok := byVar[name]
		if !ok {
			return
		}
		reachable[name] = true
		for _, dep := range declRefs(d, byVar) {
			mark(dep)
		}
	}
	for _, d := range decls {
		if d.Kind == "struct" {
			mark(d.VarName)
		}
	}
	var out []providerDecl
	for _, d := range decls {
		if reachable[d.VarName] {
			out = append(out, d)
		}
	}
	return out
}

// topoSort orders decls so each var is declared after the vars it references.
// Cycles are broken arbitrarily; they surface as Go compile errors.
func topoSort(decls []providerDecl) []providerDecl {
	byVar := map[string]providerDecl{}
	for _, d := range decls {
		byVar[d.VarName] = d
	}

	var out []providerDecl
	color := map[string]int{} // 0 white, 1 grey, 2 black
	var visit func(string)
	visit = func(name string) {
		if color[name] != 0 {
			return
		}
		color[name] = 1
		if d, ok := byVar[name]; ok {
			for _, dep := range declRefs(d, byVar) {
				visit(dep)
			}
			out = append(out, d)
		}
		color[name] = 2
	}

	names := make([]string, 0, len(byVar))
	for n := range byVar {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		visit(n)
	}
	return out
}

// pkgName returns the package qualifier of a "pkg.Type" string.
func pkgName(qualified string) string {
	if i := strings.Index(qualified, "."); i >= 0 {
		return qualified[:i]
	}
	return qualified
}

// containsIdent reports whether text contains ident as a whole Go identifier.
func containsIdent(text, ident string) bool {
	if ident == "" {
		return false
	}
	i := 0
	for {
		j := strings.Index(text[i:], ident)
		if j < 0 {
			return false
		}
		start := i + j
		end := start + len(ident)
		var before, after byte = ' ', ' '
		if start > 0 {
			before = text[start-1]
		}
		if end < len(text) {
			after = text[end]
		}
		if !isIdentByte(before) && !isIdentByte(after) {
			return true
		}
		i = end
	}
}

func isIdentByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9') || b == '_'
}

//go:embed templates/main.gen.go.tmpl
var wireTemplateText string

var wireTemplate = template.Must(template.New("main.gen.go").Parse(wireTemplateText))
