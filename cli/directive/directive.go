// Package directive implements the fabrik:cli:command generator directive.
package directive

import (
	"fmt"
	"go/ast"
	"go/doc"
	"go/token"
	"go/types"
	"strings"
	"unicode"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

const cliContextPath = "github.com/gofabrik/fabrik/cli.Context"

// New returns the CLI directive family with shared state.
func New() (*Command, *Input, *Input, *Example, *Group, *Root, *Middleware) {
	fam := newFamily()
	cmd := &Command{fam: fam}
	return cmd,
		&Input{fam: fam, kind: kindFlag},
		&Input{fam: fam, kind: kindArg},
		&Example{fam: fam},
		&Group{fam: fam},
		&Root{fam: fam},
		&Middleware{fam: fam}
}

type commandNode struct {
	pos        token.Position
	doc        string
	name       string
	path       []string
	usage      string
	aliases    []string
	hidden     bool
	middleware []string
	help       string
	long       string
	decl       ast.Node

	fn     string
	pkg    *types.Package
	params []param // parameters after ctx, classified at Emit
}

type param struct {
	name string
	typ  types.Type
}

// Command implements cli:command and associates inputs and examples at Emit.
type Command struct {
	fam *family
}

func (*Command) Name() string { return "cli:command" }

func (*Command) Meta() gen.Meta {
	return gen.Meta{
		Synopsis: "CLI command function",
		Doc: "**`//fabrik:cli:command [name=<token>] [path=\"<seg> <seg> ...\"] [usage=] [alias=<a,b>] [hidden=true] [middleware=<m1,m2>]`**\n\n" +
			"Declared on a package function `func(ctx cli.Context, deps...) error`: the first " +
			"parameter is the invocation context, the rest are injected dependencies like a " +
			"provider. The command name derives from the function name (`ServeHTTP` becomes " +
			"`serve-http`); `name=` overrides that token, and `path=` owns every segment " +
			"including the leaf, nesting the command under bare intermediate nodes " +
			"(`path=\"database migrate\"`). name= and path= are mutually exclusive; all " +
			"tokens are lowercase kebab-case, and full paths are unique per parent. " +
			"`usage=` overrides the derived usage line, `alias=` adds invocation " +
			"tokens, `hidden=true` omits the command from listings, and " +
			"`middleware=` attaches declared CLI middleware in order. " +
			"The help line is the doc comment's first sentence. The generator " +
			"emits a build function that constructs exactly this command's dependencies when " +
			"the command is selected, so help and completion never construct the application.\n\n" +
			"A bare invocation lists the commands. Runtimes are startable injected values " +
			"(`*httpserver.Server`, `*jobs.Runner`) the command starts itself; migrations are " +
			"an injectable `migrations.Sources` the command applies itself. `run()` returns an " +
			"exit code, so `main` is always `func main() { os.Exit(run()) }`.\n\n" +
			"```go\n// Start the HTTP server.\n//\n//fabrik:cli:command\nfunc Serve(ctx cli.Context, server *httpserver.Server) error {\n\treturn server.Run(ctx)\n}\n```",
		Example: "//fabrik:cli:command",
		Tier:    gen.TierBind,
		Attrs: []gen.AttrSpec{
			{Key: "name", Kind: gen.KindFreeform},
			{Key: "path", Kind: gen.KindFreeform},
			{Key: "usage", Kind: gen.KindFreeform},
			{Key: "alias", Kind: gen.KindFreeform},
			{Key: "hidden", Kind: gen.KindFreeform, Values: []string{"true", "false"}},
			{Key: "middleware", Kind: gen.KindCLIMiddlewareRef},
		},
	}
}

func (c *Command) Parse(a gen.Annotation) (any, diag.Diagnostics) {
	args, ds := gen.ParseArgs(a, c.Meta())
	if ds.HasFatal() {
		return nil, ds
	}
	nd := &commandNode{pos: a.Pos, decl: a.Decl}
	name, hasName := args.Attr["name"]
	path, hasPath := args.Attr["path"]
	if hasName && hasPath {
		ds.Error(a.Pos, "//fabrik:cli:command takes name= or path=, not both",
			"path= owns every segment including the leaf")
		return nil, ds
	}
	if hasName {
		if !tokenRE.MatchString(name.Text) {
			ds.Error(a.ArgPos(name.Col), fmt.Sprintf("invalid CLI token %q", name.Text),
				"names are lowercase kebab-case: [a-z0-9]+(-[a-z0-9]+)*")
			return nil, ds
		}
		nd.path = []string{name.Text}
	}
	if hasPath {
		segs, pds := splitPathSegments(a, path)
		ds = append(ds, pds...)
		if ds.HasFatal() {
			return nil, ds
		}
		if len(segs) == 0 {
			ds.Error(a.ArgPos(path.Col), "path= needs at least one segment", "")
			return nil, ds
		}
		nd.path = segs
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
	if fd, ok := a.Decl.(*ast.FuncDecl); ok && fd.Doc != nil {
		nd.doc = fd.Doc.Text()
	}
	return nd, ds
}

func (c *Command) Check(n any, t gen.Typed) diag.Diagnostics {
	nd := n.(*commandNode)
	fn, sig, ds := funcSig(nd.pos, t)
	if ds.HasFatal() {
		return ds
	}
	res := sig.Results()
	if res.Len() != 1 || typePath(res.At(0).Type()) != "error" {
		ds.Error(nd.pos, fmt.Sprintf("command %s has the wrong signature", fn.Name()),
			"want func(ctx cli.Context, deps...) error")
		return ds
	}
	p := sig.Params()
	if p.Len() == 0 || typePath(p.At(0).Type()) != cliContextPath {
		ds.Error(nd.pos, fmt.Sprintf("command %s must take cli.Context as its first parameter", fn.Name()),
			"want func(ctx cli.Context, deps...) error")
		return ds
	}
	// Defer classification until sibling input directives have registered.
	for i := 1; i < p.Len(); i++ {
		nd.params = append(nd.params, param{name: p.At(i).Name(), typ: p.At(i).Type()})
	}

	if len(nd.path) == 0 {
		derived := kebabCase(fn.Name())
		if !tokenRE.MatchString(derived) {
			ds.Error(nd.pos, fmt.Sprintf("function name %s derives invalid CLI token %q", fn.Name(), derived),
				"rename the function so its kebab-case form is [a-z0-9]+(-[a-z0-9]+)*")
			return ds
		}
		nd.path = []string{derived}
	}
	nd.name = nd.path[len(nd.path)-1]
	key := strings.Join(nd.path, " ")
	if first, dup := c.fam.cmdPaths[key]; dup {
		ds.Error(nd.pos, fmt.Sprintf("duplicate command %q", key),
			fmt.Sprintf("first declared at %s; command paths are unique per parent", first))
		return ds
	}
	c.fam.cmdPaths[key] = nd.pos
	c.fam.commands = append(c.fam.commands, cmdReg{path: nd.path, decl: nd.decl, fn: fn.Name(), aliases: nd.aliases, pos: nd.pos})

	nd.help, nd.long = helpAndLong(nd.doc)
	nd.fn = fn.Name()
	nd.pkg = fn.Pkg()
	return ds
}

func (c *Command) Emit(n any, g *gen.Gen) diag.Diagnostics {
	nd := n.(*commandNode)
	if nd.fn == "" {
		return nil
	}
	c.fam.consumed[nd.decl] = true
	var ds diag.Diagnostics

	ins := c.fam.inputs[nd.decl]
	byToken := map[string]*inputNode{}
	for _, in := range ins {
		if prev, dup := byToken[in.name]; dup {
			kinds := "declarations"
			if prev.kind != in.kind {
				kinds = "a flag and an argument"
			}
			ds.Error(in.pos, fmt.Sprintf("command %s declares %s named %q", nd.fn, kinds, in.name),
				"name-bound parameters cannot disambiguate; rename one")
			return ds
		}
		byToken[in.name] = in
	}
	if ads := checkArgOrder(nd.fn, ins); len(ads) > 0 {
		return ads
	}
	// Inner inputs shadow inherited ones during binding; Finish diagnoses collisions.
	type visible struct {
		in *inputNode
		v  string // shared handle, empty for command-local inputs
	}
	chain := map[string]visible{}
	if c.fam.root != nil {
		for _, in := range c.fam.inputs[c.fam.root.decl] {
			if in.kind == kindFlag {
				v, vds := c.fam.varFor(g, c.fam.root.decl, "root", in.name)
				ds = append(ds, vds...)
				chain[in.name] = visible{in: in, v: v}
			}
		}
	}
	for i := 1; i < len(nd.path); i++ {
		key := strings.Join(nd.path[:i], " ")
		for _, grp := range c.fam.groups {
			if strings.Join(grp.path, " ") != key {
				continue
			}
			for _, in := range c.fam.inputs[grp.decl] {
				if in.kind == kindFlag {
					v, vds := c.fam.varFor(g, grp.decl, strings.Join(grp.path, "-"), in.name)
					ds = append(ds, vds...)
					chain[in.name] = visible{in: in, v: v}
				}
			}
		}
	}
	for _, in := range ins {
		chain[in.name] = visible{in: in}
	}

	// After ctx, dependencies precede CLI values, and matching names always bind inputs.
	bound := map[string]*param{}
	var depTypes []types.Type
	var valueOrder []visible
	firstValue := -1
	for i := range nd.params {
		pr := &nd.params[i]
		tok := kebabCase(pr.name)
		vis, isValue := chain[tok]
		in := vis.in
		if !isValue {
			if firstValue >= 0 {
				ds.Error(nd.pos, fmt.Sprintf("CLI parameter %q must appear after dependency %q", nd.params[firstValue].name, pr.name),
					"order is ctx, dependencies, then CLI values")
				return ds
			}
			depTypes = append(depTypes, pr.typ)
			continue
		}
		if prev, dup := bound[tok]; dup {
			ds.Error(nd.pos, fmt.Sprintf("parameters %q and %q both bind CLI input %q", prev.name, pr.name, tok),
				"rename one parameter")
			return ds
		}
		if isPtr(pr.typ) {
			ds.Error(nd.pos, fmt.Sprintf("parameter %q binds CLI input %q and cannot be a pointer", pr.name, tok),
				"CLI values bind as plain values; optional arguments declare default=")
			return ds
		}
		want := inputTypes[in.typ].goType
		if typePath(pr.typ) != want {
			ds.Error(nd.pos, fmt.Sprintf("parameter %q binds CLI input %q and must have type %s (got %s)", pr.name, tok, want, typePath(pr.typ)),
				"the type= attribute decides the parameter type")
			return ds
		}
		bound[tok] = pr
		if firstValue < 0 {
			firstValue = i
		}
		valueOrder = append(valueOrder, vis)
	}
	for _, in := range ins {
		if _, ok := bound[in.name]; !ok {
			ds.Error(in.pos, fmt.Sprintf("CLI input %q has no matching parameter on %s", in.name, nd.fn),
				fmt.Sprintf("add a parameter named %q after the dependencies", lowerCamelToken(in.name)))
			return ds
		}
	}

	clip := g.Import("github.com/gofabrik/fabrik/cli")
	timePkg := ""
	for _, in := range ins {
		if in.typ == "duration" && in.hasDef {
			timePkg = g.Import("time")
		}
	}

	var inputs []gen.CommandInput
	vars := map[string]string{}
	for _, in := range ins {
		v := g.Var(lowerFirst(nd.fn) + camelToken(in.name))
		vars[in.name] = v
		inputs = append(inputs, gen.CommandInput{Var: v, Builder: builderExpr(in, clip, timePkg), Arg: in.kind == kindArg})
	}
	valueExprs := make([]string, 0, len(valueOrder))
	for _, vis := range valueOrder {
		v := vis.v
		if v == "" {
			v = vars[vis.in.name]
		}
		valueExprs = append(valueExprs, v+".Get(ctx)")
	}
	var examples []gen.CommandExample
	for _, e := range c.fam.examples[nd.decl] {
		examples = append(examples, gen.CommandExample{Cmd: e.cmd, Help: e.help})
	}

	use, mds := c.fam.resolveMiddleware(g, nd.pos, nd.middleware)
	ds = append(ds, mds...)
	if ds.HasFatal() {
		return ds
	}
	scope := g.AddScope("build"+nd.fn, nd.pos, depTypes...)
	g.AddCommandFunc(gen.CommandFunc{
		Name:       nd.name,
		Path:       nd.path,
		Help:       nd.help,
		Long:       nd.long,
		Usage:      nd.usage,
		Aliases:    nd.aliases,
		Hidden:     nd.hidden,
		Use:        use,
		Fn:         g.ImportPkg(nd.pkg) + "." + nd.fn,
		Inputs:     inputs,
		ValueExprs: valueExprs,
		Examples:   examples,
		Scope:      scope,
		Pos:        nd.pos,
	})
	return ds
}

// Finish reports unclaimed declarations and validates flag and command token collisions.
func (c *Command) Finish(g *gen.Gen) diag.Diagnostics {
	ds := c.fam.validateChains()
	ds = append(ds, c.fam.validateSiblingTokens()...)
	for _, in := range c.fam.order {
		if !c.fam.consumed[in.decl] {
			name := "cli:flag"
			if in.kind == kindArg {
				name = "cli:argument"
			}
			ds.Error(in.pos, "//fabrik:"+name+" requires //fabrik:cli:command on the same declaration", "")
		}
	}
	for _, e := range c.fam.exOrder {
		if !c.fam.consumed[e.decl] {
			ds.Error(e.pos, "//fabrik:cli:example requires //fabrik:cli:command on the same declaration", "")
		}
	}
	for _, mw := range c.fam.mwOrder {
		if !c.fam.mwReferenced[mw.name] {
			ds.Warn(mw.pos, fmt.Sprintf("CLI middleware %q is declared but never referenced", mw.name),
				"reference it with middleware= or remove the declaration")
		}
	}
	return ds
}

func checkArgOrder(fn string, ins []*inputNode) diag.Diagnostics {
	var ds diag.Diagnostics
	seenOptional := false
	for i, in := range ins {
		if in.kind != kindArg {
			continue
		}
		if in.variadic {
			for _, later := range ins[i+1:] {
				if later.kind == kindArg {
					ds.Error(in.pos, fmt.Sprintf("variadic argument %q must be the final argument of %s", in.name, fn), "")
					return ds
				}
			}
			continue
		}
		if in.required && seenOptional {
			ds.Error(in.pos, fmt.Sprintf("required argument %q follows an optional argument on %s", in.name, fn), "")
			return ds
		}
		if !in.required {
			if !in.hasDef {
				ds.Error(in.pos, fmt.Sprintf("optional argument %q needs default= on %s", in.name, fn),
					"value parameters cannot carry presence; give the argument a default")
				return ds
			}
			seenOptional = true
		}
	}
	return nil
}

// builderExpr renders one handle builder chain in canonical option order.
func builderExpr(in *inputNode, clip, timePkg string) string {
	spec := inputTypes[in.typ]
	ctor := spec.flagCtor
	if in.kind == kindArg {
		ctor = spec.argCtor
	}
	b := fmt.Sprintf("%s.%s(%q)", clip, ctor, in.name)
	if in.short != 0 {
		b += fmt.Sprintf(".Short(%q)", in.short)
	}
	if in.hasDef {
		b += ".Default(" + literalExpr(in.typ, in.def, timePkg) + ")"
	}
	if in.env != "" {
		b += fmt.Sprintf(".Env(%q)", in.env)
	}
	if len(in.values) > 0 {
		els := make([]string, len(in.values))
		for i, v := range in.values {
			els[i] = literalExpr(in.typ, v, timePkg)
		}
		b += ".OneOf(" + strings.Join(els, ", ") + ")"
	}
	if in.required {
		b += ".Required()"
	}
	if in.variadic {
		b += ".Variadic()"
	}
	if in.hidden {
		b += ".Hidden()"
	}
	if in.placeholder != "" {
		b += fmt.Sprintf(".Placeholder(%q)", in.placeholder)
	}
	if in.group != "" {
		b += fmt.Sprintf(".Group(%q)", in.group)
	}
	if in.help != "" {
		b += fmt.Sprintf(".Help(%q)", in.help)
	}
	return b
}

func isPtr(t types.Type) bool {
	_, ok := types.Unalias(t).(*types.Pointer)
	return ok
}

func lowerFirst(s string) string {
	r := []rune(s)
	r[0] = unicode.ToLower(r[0])
	return string(r)
}

func lowerCamelToken(tok string) string {
	return lowerFirst(camelToken(tok))
}

// kebabCase converts a Go identifier to a CLI token while preserving acronym runs.
func kebabCase(name string) string {
	rs := []rune(name)
	var b strings.Builder
	for i, r := range rs {
		if unicode.IsUpper(r) {
			startsWord := i > 0 && (!unicode.IsUpper(rs[i-1]) || (i+1 < len(rs) && unicode.IsLower(rs[i+1])))
			if startsWord {
				b.WriteByte('-')
			}
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// helpAndLong removes directives, derives Help with go/doc, and preserves the remaining source as Long.
func helpAndLong(text string) (help, long string) {
	var kept []string
	for _, ln := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "fabrik:") {
			continue
		}
		kept = append(kept, ln)
	}
	full := strings.TrimSpace(strings.Join(kept, "\n"))
	help = strings.TrimSuffix(new(doc.Package).Synopsis(full), ".")
	para, rest, hasRest := strings.Cut(full, "\n\n")
	long = strings.TrimSpace(para[firstSentenceEnd(para):])
	if hasRest {
		if long != "" {
			long += "\n\n"
		}
		long += strings.TrimSpace(rest)
	}
	return help, long
}

// firstSentenceEnd mirrors go/doc's sentence-boundary rules for source offsets.
func firstSentenceEnd(s string) int {
	var ppp, pp, p rune
	for i, q := range s {
		if q == '\n' || q == '\r' || q == '\t' {
			q = ' '
		}
		if q == ' ' && p == '.' && (!unicode.IsUpper(pp) || unicode.IsUpper(ppp)) {
			return i
		}
		if p == '\u3002' || p == '\uFF0E' {
			return i
		}
		ppp, pp, p = pp, p, q
	}
	return len(s)
}

func funcSig(pos token.Position, t gen.Typed) (*types.Func, *types.Signature, diag.Diagnostics) {
	var ds diag.Diagnostics
	fn, ok := t.Target.(*types.Func)
	if !ok {
		ds.Error(pos, "//fabrik:cli:command must be on a function", "")
		return nil, nil, ds
	}
	sig := fn.Signature()
	if sig.TypeParams().Len() > 0 || sig.RecvTypeParams().Len() > 0 {
		ds.Error(pos, fmt.Sprintf("//fabrik:cli:command function %s cannot be generic", fn.Name()),
			"declare a concrete function")
		return nil, nil, ds
	}
	if sig.Recv() != nil {
		ds.Error(pos, "//fabrik:cli:command must be a package function, not a method",
			"dependencies are parameters, not receiver fields")
		return nil, nil, ds
	}
	if !fn.Exported() {
		ds.Error(pos, fmt.Sprintf("//fabrik:cli:command function %s must be exported", fn.Name()),
			"generated code calls it as a package-qualified symbol; capitalize the name")
		return nil, nil, ds
	}
	if sig.Variadic() {
		ds.Error(pos, fmt.Sprintf("//fabrik:cli:command function %s cannot be variadic", fn.Name()),
			"declare each dependency as its own parameter")
		return nil, nil, ds
	}
	return fn, sig, ds
}

func typePath(t types.Type) string {
	t = types.Unalias(t)
	if p, ok := t.(*types.Pointer); ok {
		return "*" + typePath(p.Elem())
	}
	return types.TypeString(t, nil)
}
