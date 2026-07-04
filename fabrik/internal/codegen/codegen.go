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
	appPkgs   []*scan.Package // non-main packages
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
	Deps    []string // var names this decl references
	UsesCtx bool     // single provider whose signature takes context.Context

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

type provItem struct {
	pkg string
	p   *scan.Provider
}

type recvKey struct{ pkg, typ string }

func (g *generator) build() *renderContext {
	rc := &renderContext{}

	for _, pkg := range g.project.Packages {
		if pkg.Name != "main" {
			g.appPkgs = append(g.appPkgs, pkg)
		}
	}

	if g.duplicatePackages() {
		return rc // a name collision poisons every downstream lookup
	}
	g.warnMainDirectives()

	groups := g.collectProviders()
	receivers := g.collectReceivers()
	rc.ProviderDecls = orderReachable(g.buildDecls(groups, receivers))

	routes, usedPkgs := g.buildRoutes()
	rc.Routes = routes

	for _, d := range rc.ProviderDecls {
		if d.UsesCtx {
			rc.UseContext = true
			break
		}
	}

	rc.ModuleImports = g.collectImports(rc.ProviderDecls, usedPkgs)
	return rc
}

// duplicatePackages reports app packages that share a name and returns whether
// any were found. Vars and imports key on the short package name, so a collision
// poisons every downstream lookup.
func (g *generator) duplicatePackages() bool {
	byName := map[string][]*scan.Package{}
	for _, pkg := range g.appPkgs {
		byName[pkg.Name] = append(byName[pkg.Name], pkg)
	}
	found := false
	for name, pkgs := range byName {
		if len(pkgs) <= 1 {
			continue
		}
		found = true
		for _, pkg := range pkgs {
			g.diags = append(g.diags, diag.Diagnostic{
				Severity: diag.SevError, Pos: pkg.Pos,
				Message: fmt.Sprintf("duplicate package name %q", name),
				Help:    "give each package a unique name",
			})
		}
	}
	return found
}

// warnMainDirectives warns that directives in package main are not wired.
func (g *generator) warnMainDirectives() {
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
}

// collectProviders groups providers by canonical return type, reports types with
// more than one provider, and registers the generated var name for the rest.
func (g *generator) collectProviders() map[string][]provItem {
	groups := map[string][]provItem{}
	for _, pkg := range g.appPkgs {
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
	return groups
}

// collectReceivers records the handler-struct receiver of each route and
// registers its generated var name, so a struct field may depend on it.
func (g *generator) collectReceivers() map[recvKey]*scan.Route {
	receivers := map[recvKey]*scan.Route{}
	for _, pkg := range g.appPkgs {
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
	return receivers
}

// buildDecls builds the provider (constructor) and receiver-struct declarations.
func (g *generator) buildDecls(groups map[string][]provItem, receivers map[recvKey]*scan.Route) []providerDecl {
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
		args, deps, usesCtx := g.resolveArgs(it.pkg, it.p.Params)
		decls = append(decls, providerDecl{
			Kind: "single", VarName: g.providers[canon],
			PkgName: it.pkg, Func: it.p.Func, Args: args,
			Deps: deps, UsesCtx: usesCtx,
		})
	}

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
		var deps []string
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
				fdecls = append(fdecls, structField{Name: f.Name, Expr: "nil"})
				continue
			}
			fdecls = append(fdecls, structField{Name: f.Name, Expr: expr})
			deps = append(deps, expr)
		}
		decls = append(decls, providerDecl{
			Kind: "struct", VarName: providerVarName(rk.pkg, rk.typ),
			StructType: rk.pkg + "." + rk.typ, Fields: fdecls, Deps: deps,
		})
	}

	return decls
}

// buildRoutes renders each route, rejecting duplicate method+path pairs, and
// returns the set of packages the routes reference.
func (g *generator) buildRoutes() ([]routeRender, map[string]bool) {
	var routes []routeRender
	seen := map[string]*scan.Route{}
	usedPkgs := map[string]bool{}
	for _, pkg := range g.appPkgs {
		for _, r := range pkg.Routes {
			key := r.Method + " " + r.Path
			if prev, ok := seen[key]; ok {
				g.diags = append(g.diags, diag.Diagnostic{
					Severity: diag.SevError, Pos: r.Pos,
					Message: fmt.Sprintf("duplicate route %s %s", r.Method, r.Path),
					Help:    fmt.Sprintf("first declared at %s", prev.Pos),
				})
				continue
			}
			seen[key] = r
			usedPkgs[pkg.Name] = true
			routes = append(routes, routeRender{
				Method: r.Method, Path: r.Path,
				HandlerExpr: buildRouteHandler(pkg.Name, r.Func, r.Receiver),
			})
		}
	}
	return routes, usedPkgs
}

// collectImports returns the import paths of the packages referenced by the
// emitted decls and routes.
func (g *generator) collectImports(decls []providerDecl, usedPkgs map[string]bool) []string {
	for _, d := range decls {
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
	var imports []string
	for name := range usedPkgs {
		if p := importPath[name]; p != "" {
			imports = append(imports, p)
		}
	}
	sort.Strings(imports)
	return imports
}

// resolveArgs builds the call-argument string for a provider's parameters and
// reports the vars it depends on and whether it takes a context. context.Context
// params become "ctx"; other params resolve to wired vars. Unresolved params
// produce a diagnostic anchored at the provider directive.
func (g *generator) resolveArgs(pkg string, params []scan.Param) (args string, deps []string, usesCtx bool) {
	var parts []string
	for _, p := range params {
		if p.Type == "context.Context" {
			parts = append(parts, "ctx")
			usesCtx = true
			continue
		}
		v, ok := g.providers[canonicalType(pkg, p.Type)]
		if !ok {
			g.diags = append(g.diags, diag.Diagnostic{
				Severity: diag.SevError, Pos: p.Pos,
				Message: fmt.Sprintf("no provider for %s", p.Type),
				Help:    fmt.Sprintf("add a //fabrik:provider returning %s", p.Type),
			})
			parts = append(parts, "nil")
			continue
		}
		parts = append(parts, v)
		deps = append(deps, v)
	}
	return strings.Join(parts, ", "), deps, usesCtx
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

// orderReachable returns the decls reachable from the struct (handler) decls,
// each placed after the decls it depends on. Providers nothing reaches are
// dropped. Cycles are broken arbitrarily and surface as Go compile errors.
func orderReachable(decls []providerDecl) []providerDecl {
	byVar := map[string]providerDecl{}
	var roots []string
	for _, d := range decls {
		byVar[d.VarName] = d
		if d.Kind == "struct" {
			roots = append(roots, d.VarName)
		}
	}
	sort.Strings(roots)

	var out []providerDecl
	seen := map[string]bool{}
	var visit func(string)
	visit = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		d, ok := byVar[name]
		if !ok {
			return
		}
		for _, dep := range d.Deps {
			visit(dep)
		}
		out = append(out, d)
	}
	for _, r := range roots {
		visit(r)
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

//go:embed templates/main.gen.go.tmpl
var wireTemplateText string

var wireTemplate = template.Must(template.New("main.gen.go").Parse(wireTemplateText))
