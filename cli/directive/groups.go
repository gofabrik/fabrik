package directive

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

type groupNode struct {
	pos  token.Position
	decl ast.Node
	doc  string

	path       []string
	help       string
	long       string
	usage      string
	aliases    []string
	hidden     bool
	middleware []string
}

type rootNode struct {
	pos  token.Position
	decl ast.Node
	doc  string

	usage      string
	version    string
	long       string
	middleware []string
}

// genState tracks per-render handles shared by commands that bind root or group inputs.
type genState struct {
	declared map[ast.Node]declaredInputs
}

type declaredInputs struct {
	vars   map[string]string
	inputs []gen.CommandInput
}

// Group implements //fabrik:cli:group.
type Group struct{ fam *family }

func (*Group) Name() string { return "cli:group" }

func (*Group) Meta() gen.Meta {
	return gen.Meta{
		Synopsis: "Metadata for a command-tree group node",
		Doc: "**`//fabrik:cli:group name=\"<seg> [seg ...]\" [help=] [usage=] [alias=<a,b>] [hidden=true] [middleware=<m1,m2>]`**\n\n" +
			"Declared on an unexported sentinel `var _name struct{}`: describes a " +
			"non-executable tree node that `path=` segments create (help falls " +
			"back to the sentinel's doc synopsis, the remainder becomes Long). " +
			"`//fabrik:cli:flag` lines on the sentinel declare flags every " +
			"descendant command inherits and may bind; `//fabrik:cli:example` " +
			"lines attach examples; `middleware=` wraps every descendant command. " +
			"A group may not name an executable command's " +
			"path - the command owns that node.\n\n" +
			"```go\n// Database maintenance commands.\n//\n//fabrik:cli:group name=database\nvar _database struct{}\n```",
		Example: "//fabrik:cli:group name=database",
		Tier:    gen.TierBind,
		Attrs: []gen.AttrSpec{
			{Key: "name", Kind: gen.KindFreeform},
			{Key: "help", Kind: gen.KindFreeform},
			{Key: "usage", Kind: gen.KindFreeform},
			{Key: "alias", Kind: gen.KindFreeform},
			{Key: "hidden", Kind: gen.KindFreeform, Values: []string{"true", "false"}},
			{Key: "middleware", Kind: gen.KindCLIMiddlewareRef},
		},
	}
}

func (gr *Group) Parse(a gen.Annotation) (any, diag.Diagnostics) {
	args, ds := gen.ParseArgs(a, gr.Meta())
	if ds.HasFatal() {
		return nil, ds
	}
	nd := &groupNode{pos: a.Pos, decl: a.Decl, doc: annotationDoc(a)}
	name, ok := args.Attr["name"]
	if !ok {
		ds.Error(a.Pos, "//fabrik:cli:group needs name=", "example: "+gr.Meta().Example)
		return nil, ds
	}
	path, pds := splitPathSegments(a, name)
	ds = append(ds, pds...)
	if len(path) == 0 || ds.HasFatal() {
		return nil, ds
	}
	nd.path = path
	if v, ok := args.Attr["help"]; ok {
		nd.help = v.Text
	}
	if v, ok := args.Attr["usage"]; ok {
		nd.usage = v.Text
	}
	if v, ok := args.Attr["alias"]; ok {
		aliases, ads := splitAliases(a, v)
		ds = append(ds, ads...)
		if ds.HasFatal() {
			return nil, ds
		}
		nd.aliases = aliases
	}
	if v, ok := args.Attr["hidden"]; ok {
		if v.Text != "true" && v.Text != "false" {
			ds.Error(a.ArgPos(v.Col), fmt.Sprintf("hidden= wants true or false (got %q)", v.Text), "")
			return nil, ds
		}
		nd.hidden = v.Text == "true"
	}
	if v, ok := args.Attr["middleware"]; ok {
		refs, mds := splitMiddleware(a, v)
		ds = append(ds, mds...)
		if ds.HasFatal() {
			return nil, ds
		}
		nd.middleware = refs
	}
	if nd.help == "" || nd.long == "" {
		help, long := helpAndLong(nd.doc)
		if nd.help == "" {
			nd.help = help
		}
		nd.long = long
	}
	return nd, ds
}

func (gr *Group) Check(n any, t gen.Typed) diag.Diagnostics {
	nd := n.(*groupNode)
	ds := checkSentinelCarrier(nd.decl, nd.pos, t, "cli:group")
	if ds.HasFatal() {
		return ds
	}
	key := strings.Join(nd.path, " ")
	if first, dup := gr.fam.groupPaths[key]; dup {
		ds.Error(nd.pos, fmt.Sprintf("duplicate //fabrik:cli:group for %q", key),
			fmt.Sprintf("first declared at %s", first))
		return ds
	}
	gr.fam.groupPaths[key] = nd.pos
	gr.fam.groups = append(gr.fam.groups, nd)
	return ds
}

func (gr *Group) Emit(n any, g *gen.Gen) diag.Diagnostics {
	nd := n.(*groupNode)
	var ds diag.Diagnostics
	key := strings.Join(nd.path, " ")
	if _, executable := gr.fam.cmdPaths[key]; executable {
		ds.Error(nd.pos, fmt.Sprintf("//fabrik:cli:group %q names an executable command's path", key),
			"the command owns this node; groups describe non-executable nodes")
		return ds
	}
	gr.fam.consumed[nd.decl] = true
	inputs, ids := gr.fam.handlesFor(g, nd.decl, strings.Join(nd.path, "-"), true)
	ds = append(ds, ids...)
	use, mds := gr.fam.resolveMiddleware(g, nd.pos, nd.middleware)
	ds = append(ds, mds...)
	if ds.HasFatal() {
		return ds
	}
	g.AddCommandGroup(gen.CommandGroup{
		Path:     nd.path,
		Help:     nd.help,
		Long:     nd.long,
		Usage:    nd.usage,
		Aliases:  nd.aliases,
		Hidden:   nd.hidden,
		Inputs:   inputs,
		Use:      use,
		Examples: exampleSpecs(gr.fam.examples[nd.decl]),
	})
	return ds
}

// Root implements //fabrik:cli:root.
type Root struct{ fam *family }

func (*Root) Name() string { return "cli:root" }

func (*Root) Meta() gen.Meta {
	return gen.Meta{
		Synopsis: "Surface of the generated root command",
		Doc: "**`//fabrik:cli:root [usage=] [version=] [middleware=<m1,m2>]`**\n\n" +
			"Declared on an unexported sentinel `var _name struct{}`, at most " +
			"once per application: contributes the root command's usage line, " +
			"--version string, Long text (the sentinel's doc comment), root " +
			"flags (`//fabrik:cli:flag` lines, inheritable and bindable by every " +
			"command), examples, and `middleware=` applied outermost to every " +
			"invocation. The program name stays module-derived.\n\n" +
			"```go\n// The demo application.\n//\n//fabrik:cli:root version=1.0.0\nvar _root struct{}\n```",
		Example: "//fabrik:cli:root version=1.0.0",
		Tier:    gen.TierBind,
		Attrs: []gen.AttrSpec{
			{Key: "usage", Kind: gen.KindFreeform},
			{Key: "version", Kind: gen.KindFreeform},
			{Key: "middleware", Kind: gen.KindCLIMiddlewareRef},
		},
	}
}

func (r *Root) Parse(a gen.Annotation) (any, diag.Diagnostics) {
	args, ds := gen.ParseArgs(a, r.Meta())
	if ds.HasFatal() {
		return nil, ds
	}
	nd := &rootNode{pos: a.Pos, decl: a.Decl, doc: annotationDoc(a)}
	if v, ok := args.Attr["usage"]; ok {
		nd.usage = v.Text
	}
	if v, ok := args.Attr["version"]; ok {
		nd.version = v.Text
	}
	if v, ok := args.Attr["middleware"]; ok {
		refs, mds := splitMiddleware(a, v)
		ds = append(ds, mds...)
		if ds.HasFatal() {
			return nil, ds
		}
		nd.middleware = refs
	}
	nd.long = strippedDoc(nd.doc)
	return nd, ds
}

func (r *Root) Check(n any, t gen.Typed) diag.Diagnostics {
	nd := n.(*rootNode)
	ds := checkSentinelCarrier(nd.decl, nd.pos, t, "cli:root")
	if ds.HasFatal() {
		return ds
	}
	if r.fam.root != nil {
		ds.Error(nd.pos, "duplicate //fabrik:cli:root",
			fmt.Sprintf("first declared at %s; an application has one root", r.fam.root.pos))
		return ds
	}
	r.fam.root = nd
	return ds
}

func (r *Root) Emit(n any, g *gen.Gen) diag.Diagnostics {
	nd := n.(*rootNode)
	var ds diag.Diagnostics
	r.fam.consumed[nd.decl] = true
	if nd.version != "" {
		for _, in := range r.fam.inputs[nd.decl] {
			if in.kind == kindFlag && in.name == "version" {
				ds.Error(in.pos, "root flag \"version\" collides with the --version flag version= reserves",
					"the cli library rejects this tree at startup")
				return ds
			}
		}
	}
	inputs, ids := r.fam.handlesFor(g, nd.decl, "root", true)
	ds = append(ds, ids...)
	use, mds := r.fam.resolveMiddleware(g, nd.pos, nd.middleware)
	ds = append(ds, mds...)
	if ds.HasFatal() {
		return ds
	}
	g.SetCommandRoot(gen.RootSpec{
		Usage:    nd.usage,
		Version:  nd.version,
		Long:     nd.long,
		Inputs:   inputs,
		Use:      use,
		Examples: exampleSpecs(r.fam.examples[nd.decl]),
	})
	return ds
}

// handlesFor allocates inputs once per render so inherited bindings share handles.
func (f *family) handlesFor(g *gen.Gen, decl ast.Node, prefix string, flagsOnly bool) ([]gen.CommandInput, diag.Diagnostics) {
	st := f.state(g)
	if cached, ok := st.declared[decl]; ok {
		return cached.inputs, nil
	}
	vars, inputs, ds := f.declareInputs(g, prefix, f.inputs[decl], flagsOnly)
	st.declared[decl] = declaredInputs{vars: vars, inputs: inputs}
	return inputs, ds
}

// varFor returns a shared handle and its one-time allocation diagnostics, which the caller must report.
func (f *family) varFor(g *gen.Gen, decl ast.Node, prefix, name string) (string, diag.Diagnostics) {
	st := f.state(g)
	var ds diag.Diagnostics
	if _, ok := st.declared[decl]; !ok {
		_, ds = f.handlesFor(g, decl, prefix, true)
	}
	return st.declared[decl].vars[name], ds
}

// declareInputs rejects positional inputs when flagsOnly is set.
func (f *family) declareInputs(g *gen.Gen, prefix string, ins []*inputNode, flagsOnly bool) (map[string]string, []gen.CommandInput, diag.Diagnostics) {
	var ds diag.Diagnostics
	clip := g.Import("github.com/gofabrik/fabrik/cli")
	timePkg := ""
	for _, in := range ins {
		if in.typ == "duration" && in.hasDef {
			timePkg = g.Import("time")
		}
	}
	vars := map[string]string{}
	var inputs []gen.CommandInput
	for _, in := range ins {
		if flagsOnly && in.kind == kindArg {
			ds.Error(in.pos, "//fabrik:cli:argument belongs to a command function",
				"groups and the root declare flags only")
			continue
		}
		v := g.Var(lowerCamelToken(prefix) + camelToken(in.name))
		vars[in.name] = v
		inputs = append(inputs, gen.CommandInput{Var: v, Builder: builderExpr(in, clip, timePkg), Arg: in.kind == kindArg})
	}
	return vars, inputs, ds
}

func exampleSpecs(exs []*exampleNode) []gen.CommandExample {
	var out []gen.CommandExample
	for _, e := range exs {
		out = append(out, gen.CommandExample{Cmd: e.cmd, Help: e.help})
	}
	return out
}

// strippedDoc removes directive lines from a doc comment.
func strippedDoc(text string) string {
	var kept []string
	for _, ln := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "fabrik:") {
			continue
		}
		kept = append(kept, ln)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// annotationDoc reconstructs docs because sentinel annotations attach to the GenDecl.
func annotationDoc(a gen.Annotation) string {
	var b strings.Builder
	for _, ln := range a.Doc {
		b.WriteString(strings.TrimPrefix(strings.TrimPrefix(ln, "//"), " "))
		b.WriteString("\n")
	}
	return b.String()
}

// checkSentinelCarrier requires an uninitialized, unexported underscore-prefixed var of type struct{}.
func checkSentinelCarrier(nd ast.Node, pos token.Position, t gen.Typed, name string) diag.Diagnostics {
	var ds diag.Diagnostics
	bad := func() diag.Diagnostics {
		ds.Error(pos, "//fabrik:"+name+" must be on an unexported sentinel variable",
			"declare it as: var _name struct{}")
		return ds
	}
	v, ok := t.Target.(*types.Var)
	if !ok || v.Exported() || !strings.HasPrefix(v.Name(), "_") {
		return bad()
	}
	vs, ok := nd.(*ast.ValueSpec)
	if !ok || len(vs.Names) != 1 || len(vs.Values) != 0 {
		return bad()
	}
	st, ok := vs.Type.(*ast.StructType)
	if !ok || st.Fields == nil || len(st.Fields.List) != 0 {
		return bad()
	}
	return ds
}

func splitPathSegments(a gen.Annotation, attr gen.Arg) ([]string, diag.Diagnostics) {
	var ds diag.Diagnostics
	var path []string
	rest, off := attr.Text, 0
	for rest != "" {
		trimmed := strings.TrimLeft(rest, " ")
		off += len(rest) - len(trimmed)
		if trimmed == "" {
			break
		}
		seg, tail, _ := strings.Cut(trimmed, " ")
		if !tokenRE.MatchString(seg) {
			ds.Error(a.ArgPos(attr.Col+off), fmt.Sprintf("invalid CLI token %q in path", seg),
				"segments are lowercase kebab-case: [a-z0-9]+(-[a-z0-9]+)*")
			return nil, ds
		}
		path = append(path, seg)
		off += len(seg) + 1
		rest = tail
	}
	return path, ds
}

func splitAliases(a gen.Annotation, attr gen.Arg) ([]string, diag.Diagnostics) {
	var ds diag.Diagnostics
	var out []string
	for _, alias := range strings.Split(attr.Text, ",") {
		if !tokenRE.MatchString(alias) {
			ds.Error(a.ArgPos(attr.Col), fmt.Sprintf("invalid CLI token %q in alias=", alias),
				"aliases are single lowercase kebab-case tokens")
			return nil, ds
		}
		out = append(out, alias)
	}
	return out, ds
}

type chainOwner struct {
	path  []string
	label string
	ins   []*inputNode
	cmd   bool
}

// validateChains reports inherited name and short-flag collisions once at the inner declaration.
func (f *family) validateChains() diag.Diagnostics {
	var ds diag.Diagnostics
	owners := []chainOwner{}
	if f.root != nil {
		owners = append(owners, chainOwner{path: nil, label: "the root", ins: f.inputs[f.root.decl]})
	}
	for _, grp := range f.groups {
		if _, executable := f.cmdPaths[strings.Join(grp.path, " ")]; executable {
			continue
		}
		owners = append(owners, chainOwner{path: grp.path, label: "group " + strings.Join(grp.path, " "), ins: f.inputs[grp.decl]})
	}
	for _, cmd := range f.commands {
		owners = append(owners, chainOwner{path: cmd.path, label: cmd.fn, ins: f.inputs[cmd.decl], cmd: true})
	}

	reported := map[*inputNode]bool{}
	for _, owner := range owners {
		shorts := map[rune]*inputNode{}
		names := map[string]*inputNode{}
		for _, in := range owner.ins {
			if in.kind != kindFlag {
				continue
			}
			if _, dup := names[in.name]; dup && !owner.cmd {
				// Emit reports command-local duplicates.
				ds.Error(in.pos, fmt.Sprintf("%s declares two flags named %q", owner.label, in.name), "")
				reported[in] = true
				continue
			}
			names[in.name] = in
			if in.short == 0 {
				continue
			}
			if prev, dup := shorts[in.short]; dup {
				ds.Error(in.pos, fmt.Sprintf("flags %q and %q share short -%c on %s", prev.name, in.name, in.short, owner.label),
					"the cli library rejects duplicate shorts at startup")
				reported[in] = true
				continue
			}
			shorts[in.short] = in
		}
	}
	for _, inner := range owners {
		for _, outer := range owners {
			if !isPathPrefix(outer.path, inner.path) {
				continue
			}
			for _, in := range inner.ins {
				if in.kind != kindFlag || reported[in] {
					continue
				}
				for _, anc := range outer.ins {
					if anc.kind != kindFlag {
						continue
					}
					switch {
					case anc.name == in.name:
						ds.Error(in.pos, fmt.Sprintf("flag %q collides with the inherited flag declared at %s", in.name, anc.pos),
							"inherited long names are unique across the ancestor chain")
						reported[in] = true
					case anc.short != 0 && anc.short == in.short:
						ds.Error(in.pos, fmt.Sprintf("flags %q and %q share short -%c on one chain", anc.name, in.name, in.short),
							"the cli library rejects duplicate shorts at startup")
						reported[in] = true
					}
				}
			}
		}
	}
	return ds
}

func isPathPrefix(outer, inner []string) bool {
	if len(outer) >= len(inner) {
		return false
	}
	for i, seg := range outer {
		if inner[i] != seg {
			return false
		}
	}
	return true
}

// validateSiblingTokens requires unique canonical names, intermediate nodes, and aliases among siblings.
func (f *family) validateSiblingTokens() diag.Diagnostics {
	var ds diag.Diagnostics
	canonical := map[string]map[string]bool{}
	claimName := func(parent, tok string) {
		if canonical[parent] == nil {
			canonical[parent] = map[string]bool{}
		}
		canonical[parent][tok] = true
	}
	addPathNodes := func(path []string) {
		for i := 1; i <= len(path); i++ {
			claimName(strings.Join(path[:i-1], " "), path[i-1])
		}
	}
	for _, cmd := range f.commands {
		addPathNodes(cmd.path)
	}
	for _, grp := range f.groups {
		addPathNodes(grp.path)
	}

	aliases := map[string]map[string]token.Position{}
	claimAlias := func(parent, alias string, pos token.Position) {
		if canonical[parent][alias] {
			ds.Error(pos, fmt.Sprintf("alias %q collides with a sibling command or group", alias),
				"the cli library requires sibling names and aliases to be unique")
			return
		}
		if aliases[parent] == nil {
			aliases[parent] = map[string]token.Position{}
		}
		if first, dup := aliases[parent][alias]; dup {
			ds.Error(pos, fmt.Sprintf("alias %q is already used by a sibling declared at %s", alias, first), "")
			return
		}
		aliases[parent][alias] = pos
	}
	for _, cmd := range f.commands {
		parent := strings.Join(cmd.path[:len(cmd.path)-1], " ")
		for _, a := range cmd.aliases {
			claimAlias(parent, a, cmd.pos)
		}
	}
	for _, grp := range f.groups {
		parent := strings.Join(grp.path[:len(grp.path)-1], " ")
		for _, a := range grp.aliases {
			claimAlias(parent, a, grp.pos)
		}
	}
	return ds
}
