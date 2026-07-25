package directive

import (
	"fmt"
	"go/token"
	"go/types"
	"sort"
	"strings"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

// Globals implements //fabrik:templates:global.
type Globals struct {
	templates *Templates
}

func NewGlobals(t *Templates) *Globals { return &Globals{templates: t} }

func (*Globals) Name() string { return "templates:global" }

func (*Globals) Meta() gen.Meta {
	return gen.Meta{
		Synopsis: "Request-scoped template function: [name=NAME]",
		Doc: "**`//fabrik:templates:global [name=NAME]`**\n\n" +
			"Adds a function that reads the context of the render calling " +
			"it, so layouts reach request-scoped values without page " +
			"structs or handlers carrying them. The first parameter must " +
			"be a `context.Context`; every parameter after it is an " +
			"argument the template passes. Dependencies come from a " +
			"receiver struct wired like a handler struct, so a global " +
			"needing none stays a package-level function.\n\n" +
			"The template-visible name defaults to the function name with " +
			"a lowered first letter (`CurrentUser` -> `currentUser`); " +
			"`name=` overrides. The result must be legal for the template " +
			"engines: one value, or two with the second an `error`.\n\n" +
			"```go\n//fabrik:templates:global\n" +
			"func (h *ViewHelpers) CurrentUser(ctx context.Context) (*Session, error) { ... }\n```",
		Example: "//fabrik:templates:global",
		Attrs: []gen.AttrSpec{
			{Key: "name", Kind: gen.KindFreeform},
		},
	}
}

type globalNode struct {
	pos  token.Position
	name string

	fn      string
	pkg     *types.Package
	recv    types.Type // nil for a package-level function
	fset    *token.FileSet
	params  []*types.Var // template arguments, after the context
	results *types.Tuple
}

func (gl *Globals) Parse(a gen.Annotation) (any, diag.Diagnostics) {
	args, ds := gen.ParseArgs(a, gl.Meta())
	nd := &globalNode{pos: a.Pos}
	if nm, ok := args.Attr["name"]; ok {
		nd.name = nm.Text
		if !isFuncName(nd.name) {
			ds.Error(a.ArgPos(nm.Col), fmt.Sprintf("invalid template function name %q", nd.name),
				"use a short identifier: name=currentUser")
		}
	}
	if ds.HasFatal() {
		return nil, ds
	}
	return nd, ds
}

func (gl *Globals) Check(n any, ty gen.Typed) diag.Diagnostics {
	nd := n.(*globalNode)
	var ds diag.Diagnostics

	fn, ok := ty.Target.(*types.Func)
	if !ok {
		ds.Error(nd.pos, "//fabrik:templates:global must be on a function", "")
		return ds
	}
	sig := fn.Signature()
	if !fn.Exported() {
		ds.Error(nd.pos, fmt.Sprintf("template global %s is unexported", fn.Name()),
			"generated code lives in package main; export it")
		return ds
	}
	if sig.TypeParams().Len() > 0 || sig.RecvTypeParams().Len() > 0 {
		ds.Error(nd.pos, fmt.Sprintf("template global %s cannot be generic (generated code cannot infer type arguments)", fn.Name()),
			"declare a concrete function")
		return ds
	}
	if sig.Variadic() {
		ds.Error(nd.pos, fmt.Sprintf("template global %s cannot be variadic", fn.Name()),
			"take the arguments the template passes as fixed parameters")
		return ds
	}
	if recv := sig.Recv(); recv != nil {
		obj, ok := gl.templates.receiverInfo(recv.Type())
		if !ok {
			ds.Error(nd.pos, fmt.Sprintf("template global receiver %s is not a struct",
				types.TypeString(recv.Type(), types.RelativeTo(fn.Pkg()))),
				"declare globals on a struct whose fields fabrik wires, or on a package-level function")
			return ds
		}
		if !obj.Exported() {
			ds.Error(nd.pos, fmt.Sprintf("template global receiver %s is unexported", obj.Name()),
				"generated code lives in package main; export the type")
			return ds
		}
		nd.recv = recv.Type()
	}
	params := sig.Params()
	if params.Len() == 0 || !isContext(params.At(0).Type()) {
		ds.Error(nd.pos, fmt.Sprintf("template global %s must take a context.Context first", fn.Name()),
			"globals read the context of the render calling them")
		return ds
	}
	if !legalFuncSignature(sig) {
		ds.Error(nd.pos, fmt.Sprintf("template global %s has an illegal signature", fn.Name()),
			"html/template functions return one value, or two with the second an error")
		return ds
	}
	for i := 1; i < params.Len(); i++ {
		nd.params = append(nd.params, params.At(i))
	}
	for _, v := range append(append([]*types.Var{}, nd.params...), tupleVars(sig.Results())...) {
		if what, ok := unexportedNamed(v.Type()); ok {
			ds.Error(nd.pos, fmt.Sprintf("template global %s uses unexported %s", fn.Name(), what),
				"generated code lives in package main; export the type")
			return ds
		}
	}
	nd.results = sig.Results()

	if nd.name == "" {
		nd.name = gen.LowerFirst(fn.Name())
	}
	if pos, dup := gl.templates.funcNamePos(nd.name); dup {
		ds.Error(nd.pos, fmt.Sprintf("duplicate template function name %q", nd.name),
			fmt.Sprintf("first declared at %s", pos))
		return ds
	}
	nd.fn = fn.Name()
	nd.pkg = fn.Pkg()
	nd.fset = ty.Fset
	gl.templates.addGlobal(nd)
	return ds
}

// PrepareNode registers receiver bindings before dependency resolution.
func (*Globals) PrepareNode(n any, g *gen.Gen) {
	nd := n.(*globalNode)
	if nd.recv == nil {
		return
	}
	gen.RegisterStruct(g, nd.fset, receiverPointer(nd.recv))
}

func (*Globals) Emit(any, *gen.Gen) diag.Diagnostics { return nil }

// ContributeGlobal registers one FuncMap entry for the Globals
// builder, letting a library ship a request-scoped template function.
// bind names the *templates.Binding in scope, so the entry reads
// bind + ".Ctx()". An app declaring the same name keeps its own
// declaration and the contribution is skipped.
func (t *Templates) ContributeGlobal(name string, build func(g *gen.Gen, bind string) (string, diag.Diagnostics)) {
	t.contributedGlobals = append(t.contributedGlobals, globalContribution{name: name, build: build})
}

// bindPlaceholder stands in for the binding's name while contributions
// render, since the name depends on what they turn out to use.
const bindPlaceholder = "fabrikBinding"

type globalContribution struct {
	name  string
	build func(g *gen.Gen, bind string) (string, diag.Diagnostics)
}

// contributedGlobalEntries renders library entries, sorted so
// generation stays deterministic.
func (t *Templates) contributedGlobalEntries(g *gen.Gen, bind string) (string, diag.Diagnostics) {
	var ds diag.Diagnostics
	var entries []string
	for _, c := range t.contributedGlobals {
		if _, taken := t.byName[c.name]; taken {
			continue
		}
		expr, cds := c.build(g, bind)
		ds = append(ds, cds...)
		if expr != "" && !cds.HasFatal() {
			entries = append(entries, expr)
		}
	}
	sort.Strings(entries)
	return strings.Join(entries, ""), ds
}

func (t *Templates) globalsExpr(g *gen.Gen, tplPkg string) (string, diag.Diagnostics) {
	if len(t.globals) == 0 && len(t.contributedGlobals) == 0 {
		return "", nil
	}

	sorted := append([]*globalNode(nil), t.globals...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].name < sorted[j].name })

	var ds diag.Diagnostics
	callees := make([]string, len(sorted))
	types := make([][]string, len(sorted))
	// The binding name must not shadow a wrapper qualifier.
	qualifiers := map[string]bool{tplPkg: true}
	for i, nd := range sorted {
		callee, cds := globalCallee(g, nd)
		ds = append(ds, cds...)
		callees[i] = callee
		qualifiers[firstIdent(callee)] = true
		types[i] = typeExprs(g, nd)
		for _, expr := range types[i] {
			for _, q := range exprQualifiers(expr) {
				qualifiers[q] = true
			}
		}
	}
	// Each contribution runs once, against a placeholder, so its
	// diagnostics and side effects happen once; the binding's real name
	// replaces the placeholder after the qualifiers it uses are known.
	contributed, cds := t.contributedGlobalEntries(g, bindPlaceholder)
	ds = append(ds, cds...)
	if cds.HasFatal() {
		return "", ds
	}
	if len(t.globals) == 0 && contributed == "" {
		// Every contributed name was taken by an app declaration, so
		// nothing request-scoped is left to build.
		return "", ds
	}
	for _, q := range exprQualifiers(contributed) {
		if q != bindPlaceholder {
			qualifiers[q] = true
		}
	}
	bind := "b"
	for qualifiers[bind] {
		bind += "_"
	}
	contributed = strings.ReplaceAll(contributed, bindPlaceholder, bind)

	var out strings.Builder
	fmt.Fprintf(&out, "%s.Globals(func(%s *%s.Binding) %s.FuncMap {\n", tplPkg, bind, tplPkg, tplPkg)
	fmt.Fprintf(&out, "return %s.FuncMap{\n", tplPkg)
	out.WriteString(contributed)
	for i, nd := range sorted {
		callee := callees[i]
		names := wrapperNames(nd, bind, callee, firstIdent(callee))
		fmt.Fprintf(&out, "%q: func(%s) %s { return %s(%s.Ctx()%s) },\n",
			nd.name, params(types[i], names), results(types[i], len(names)),
			callee, bind, joinArgs(names))
	}
	out.WriteString("}\n})")
	return out.String(), ds
}

func joinArgs(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return ", " + strings.Join(names, ", ")
}

func globalCallee(g *gen.Gen, nd *globalNode) (string, diag.Diagnostics) {
	if nd.recv == nil {
		return g.ImportPkg(nd.pkg) + "." + nd.fn, nil
	}
	inst, ds := gen.EnsureStruct(g, nd.fset, receiverPointer(nd.recv))
	return inst + "." + nd.fn, ds
}

func firstIdent(call string) string {
	if i := strings.IndexByte(call, '.'); i >= 0 {
		return call[:i]
	}
	return call
}

func typeExprs(g *gen.Gen, nd *globalNode) []string {
	out := make([]string, 0, len(nd.params)+nd.results.Len())
	for _, p := range nd.params {
		out = append(out, g.TypeExpr(p.Type()))
	}
	for _, v := range tupleVars(nd.results) {
		out = append(out, g.TypeExpr(v.Type()))
	}
	return out
}

func exprQualifiers(expr string) []string {
	var out []string
	for i := 0; i < len(expr); i++ {
		if expr[i] != '.' {
			continue
		}
		start := i
		for start > 0 && isIdentByte(expr[start-1]) {
			start--
		}
		if start < i {
			out = append(out, expr[start:i])
		}
	}
	return out
}

func isIdentByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func params(exprs, names []string) string {
	parts := make([]string, 0, len(names))
	for i, name := range names {
		parts = append(parts, name+" "+exprs[i])
	}
	return strings.Join(parts, ", ")
}

func results(exprs []string, paramCount int) string {
	parts := exprs[paramCount:]
	if len(parts) == 1 {
		return parts[0]
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

// wrapperNames replaces unusable names and names that shadow wrapper identifiers.
func wrapperNames(nd *globalNode, reserved ...string) []string {
	blocked := map[string]bool{}
	for _, r := range reserved {
		blocked[r] = true
	}
	taken := map[string]bool{}
	for _, p := range nd.params {
		if name := p.Name(); name != "" && name != "_" && !blocked[name] {
			taken[name] = true
		}
	}
	names := make([]string, len(nd.params))
	for i, p := range nd.params {
		if name := p.Name(); name != "" && name != "_" && !blocked[name] {
			names[i] = name
			continue
		}
		name := fmt.Sprintf("a%d", i)
		for taken[name] || blocked[name] {
			name += "_"
		}
		taken[name] = true
		names[i] = name
	}
	return names
}

func tupleVars(t *types.Tuple) []*types.Var {
	out := make([]*types.Var, 0, t.Len())
	for i := 0; i < t.Len(); i++ {
		out = append(out, t.At(i))
	}
	return out
}

func isContext(t types.Type) bool {
	named, ok := types.Unalias(t).(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	return obj.Pkg() != nil && obj.Pkg().Path() == "context" && obj.Name() == "Context"
}

// unexportedNamed reports the first part of t that generated package-main code cannot name.
func unexportedNamed(t types.Type) (string, bool) {
	// Exported aliases hide their targets, but not their type arguments.
	if a, ok := t.(*types.Alias); ok {
		if obj := a.Obj(); obj.Pkg() != nil && !obj.Exported() {
			return "type " + obj.Name(), true
		}
		for i := 0; i < a.TypeArgs().Len(); i++ {
			if what, ok := unexportedNamed(a.TypeArgs().At(i)); ok {
				return what, true
			}
		}
		return "", false
	}
	switch u := t.(type) {
	case *types.Named:
		if obj := u.Obj(); obj.Pkg() != nil && !obj.Exported() {
			return "type " + obj.Name(), true
		}
		for i := 0; i < u.TypeArgs().Len(); i++ {
			if name, ok := unexportedNamed(u.TypeArgs().At(i)); ok {
				return name, true
			}
		}
	case *types.Pointer:
		return unexportedNamed(u.Elem())
	case *types.Slice:
		return unexportedNamed(u.Elem())
	case *types.Array:
		return unexportedNamed(u.Elem())
	case *types.Map:
		if name, ok := unexportedNamed(u.Key()); ok {
			return name, true
		}
		return unexportedNamed(u.Elem())
	case *types.Chan:
		return unexportedNamed(u.Elem())
	case *types.Signature:
		for _, v := range append(tupleVars(u.Params()), tupleVars(u.Results())...) {
			if name, ok := unexportedNamed(v.Type()); ok {
				return name, true
			}
		}
	case *types.Struct:
		for i := 0; i < u.NumFields(); i++ {
			f := u.Field(i)
			if !f.Exported() {
				return "field " + f.Name(), true
			}
			if name, ok := unexportedNamed(f.Type()); ok {
				return name, true
			}
		}
	case *types.Interface:
		for i := 0; i < u.NumExplicitMethods(); i++ {
			m := u.ExplicitMethod(i)
			if !m.Exported() {
				return "method " + m.Name(), true
			}
			if name, ok := unexportedNamed(m.Signature()); ok {
				return name, true
			}
		}
		for i := 0; i < u.NumEmbeddeds(); i++ {
			if name, ok := unexportedNamed(u.EmbeddedType(i)); ok {
				return name, true
			}
		}
	}
	return "", false
}

func receiverPointer(recv types.Type) types.Type {
	t := types.Unalias(recv)
	if _, ok := t.(*types.Pointer); !ok {
		t = types.NewPointer(t)
	}
	return t
}
