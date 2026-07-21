package directive

import (
	"fmt"
	"go/ast"
	"go/token"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

var tokenRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

type inputSpec struct {
	flagCtor  string
	argCtor   string // empty when the type is flag-only
	goType    string
	defaultOK bool
	valuesOK  bool
	canon     func(string) (string, error) // canonicalizes an attribute as a Go literal
}

var inputTypes = map[string]inputSpec{
	"string":   {flagCtor: "StringFlag", argCtor: "StringArg", goType: "string", defaultOK: true, valuesOK: true, canon: func(s string) (string, error) { return strconv.Quote(s), nil }},
	"bool":     {flagCtor: "BoolFlag", goType: "bool", defaultOK: true, canon: canonBool},
	"int":      {flagCtor: "IntFlag", argCtor: "IntArg", goType: "int", defaultOK: true, valuesOK: true, canon: canonInt},
	"int64":    {flagCtor: "Int64Flag", goType: "int64", defaultOK: true, valuesOK: true, canon: canonInt},
	"float64":  {flagCtor: "FloatFlag", goType: "float64", defaultOK: true, valuesOK: true, canon: canonFloat},
	"duration": {flagCtor: "DurationFlag", goType: "time.Duration", defaultOK: true, canon: canonDuration},
	"time":     {flagCtor: "TimeFlag", goType: "time.Time"},
	"strings":  {flagCtor: "StringSliceFlag", argCtor: "StringSliceArg", goType: "[]string"},
	"ints":     {flagCtor: "IntSliceFlag", goType: "[]int"},
}

func canonBool(s string) (string, error) {
	if s != "true" && s != "false" {
		return "", fmt.Errorf("want true or false")
	}
	return s, nil
}

func canonInt(s string) (string, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(n, 10), nil
}

func canonFloat(s string) (string, error) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return "", err
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "", fmt.Errorf("NaN and Inf have no Go literal")
	}
	return strconv.FormatFloat(f, 'g', -1, 64), nil
}

// canonDuration defers literal rendering until the import alias is known.
func canonDuration(s string) (string, error) {
	if _, err := time.ParseDuration(s); err != nil {
		return "", err
	}
	return s, nil
}

type inputKind int

const (
	kindFlag inputKind = iota
	kindArg
)

type inputNode struct {
	kind inputKind
	pos  token.Position
	decl ast.Node

	name, typ, help, env, placeholder, group string
	short                                    rune
	required, variadic, hidden               bool
	def                                      string
	hasDef                                   bool
	values                                   []string
}

type exampleNode struct {
	pos       token.Position
	decl      ast.Node
	cmd, help string
}

// family groups directives by declaration for Command to consume at Emit.
type family struct {
	inputs   map[ast.Node][]*inputNode
	examples map[ast.Node][]*exampleNode
	order    []*inputNode // registration order for unconsumed diagnostics
	exOrder  []*exampleNode
	consumed map[ast.Node]bool

	cmdPaths     map[string]token.Position
	commands     []cmdReg
	groups       []*groupNode
	groupPaths   map[string]token.Position
	root         *rootNode
	middlewares  map[string]*mwNode
	mwOrder      []*mwNode
	mwReferenced map[string]bool
	perGen       map[*gen.Gen]*genState
}

func newFamily() *family {
	return &family{
		inputs:       map[ast.Node][]*inputNode{},
		examples:     map[ast.Node][]*exampleNode{},
		consumed:     map[ast.Node]bool{},
		cmdPaths:     map[string]token.Position{},
		groupPaths:   map[string]token.Position{},
		middlewares:  map[string]*mwNode{},
		mwReferenced: map[string]bool{},
		perGen:       map[*gen.Gen]*genState{},
	}
}

type cmdReg struct {
	path    []string
	decl    ast.Node
	fn      string
	aliases []string
	pos     token.Position
}

// state isolates handle allocation between validation and rendering passes.
func (f *family) state(g *gen.Gen) *genState {
	st, ok := f.perGen[g]
	if !ok {
		st = &genState{declared: map[ast.Node]declaredInputs{}}
		f.perGen[g] = st
	}
	return st
}

// Input implements //fabrik:cli:flag and //fabrik:cli:argument.
type Input struct {
	fam  *family
	kind inputKind
}

func (i *Input) Name() string {
	if i.kind == kindArg {
		return "cli:argument"
	}
	return "cli:flag"
}

func (i *Input) Meta() gen.Meta {
	if i.kind == kindArg {
		return gen.Meta{
			Synopsis: "Positional argument of a CLI command",
			Doc: "**`//fabrik:cli:argument name= type= [help=] [default=] [values=] [required=true] [variadic=true]`**\n\n" +
				"Declared alongside `//fabrik:cli:command` on the same function: one " +
				"positional argument, bound to the plain parameter whose name matches " +
				"(`direction` binds `direction string`). Declaration order is binding " +
				"order. Optional arguments need `default=`; `values=` restricts and " +
				"completes; `variadic=true` (type `strings`) collects the tail.\n\n" +
				"```go\n//fabrik:cli:command\n//fabrik:cli:argument name=direction type=string values=up,down default=up help=\"Migration direction.\"\nfunc Migrate(ctx cli.Context, db *sql.DB, direction string) error { ... }\n```",
			Example: "//fabrik:cli:argument name=direction type=string default=up",
			Tier:    gen.TierBind,
			Attrs: []gen.AttrSpec{
				{Key: "name", Kind: gen.KindFreeform},
				{Key: "type", Kind: gen.KindFreeform, Values: []string{"string", "int", "strings"}},
				{Key: "help", Kind: gen.KindFreeform},
				{Key: "default", Kind: gen.KindFreeform},
				{Key: "values", Kind: gen.KindFreeform},
				{Key: "required", Kind: gen.KindFreeform, Values: []string{"true", "false"}},
				{Key: "variadic", Kind: gen.KindFreeform, Values: []string{"true", "false"}},
			},
		}
	}
	return gen.Meta{
		Synopsis: "Flag of a CLI command",
		Doc: "**`//fabrik:cli:flag name= type= [short=] [help=] [default=] [values=] [env=] [required=true] [hidden=true] [placeholder=] [group=]`**\n\n" +
			"Declared alongside `//fabrik:cli:command` on the same function: one " +
			"flag, bound to the plain parameter whose lowerCamel name matches the " +
			"kebab-case flag (`dry-run` binds `dryRun bool`). The cli library owns " +
			"parsing, defaults, validation, and completion; `values=` maps to " +
			"`OneOf` on scalar types.\n\n" +
			"```go\n//fabrik:cli:command\n//fabrik:cli:flag name=dry-run short=n type=bool help=\"Print without applying.\"\nfunc Migrate(ctx cli.Context, db *sql.DB, dryRun bool) error { ... }\n```",
		Example: "//fabrik:cli:flag name=dry-run type=bool",
		Tier:    gen.TierBind,
		Attrs: []gen.AttrSpec{
			{Key: "name", Kind: gen.KindFreeform},
			{Key: "type", Kind: gen.KindFreeform, Values: []string{"string", "bool", "int", "int64", "float64", "duration", "time", "strings", "ints"}},
			{Key: "short", Kind: gen.KindFreeform},
			{Key: "help", Kind: gen.KindFreeform},
			{Key: "default", Kind: gen.KindFreeform},
			{Key: "values", Kind: gen.KindFreeform},
			{Key: "env", Kind: gen.KindFreeform},
			{Key: "required", Kind: gen.KindFreeform, Values: []string{"true", "false"}},
			{Key: "hidden", Kind: gen.KindFreeform, Values: []string{"true", "false"}},
			{Key: "placeholder", Kind: gen.KindFreeform},
			{Key: "group", Kind: gen.KindFreeform},
		},
	}
}

func (i *Input) Parse(a gen.Annotation) (any, diag.Diagnostics) {
	args, ds := gen.ParseArgs(a, i.Meta())
	if ds.HasFatal() {
		return nil, ds
	}
	directive := "//fabrik:" + i.Name()
	nd := &inputNode{kind: i.kind, pos: a.Pos, decl: a.Decl}

	name, ok := args.Attr["name"]
	if !ok {
		ds.Error(a.Pos, directive+" needs name=", "example: "+i.Meta().Example)
		return nil, ds
	}
	if !tokenRE.MatchString(name.Text) {
		ds.Error(a.ArgPos(name.Col), fmt.Sprintf("invalid CLI token %q", name.Text),
			"names are lowercase kebab-case: [a-z0-9]+(-[a-z0-9]+)*")
		return nil, ds
	}
	nd.name = name.Text

	typ, ok := args.Attr["type"]
	if !ok {
		ds.Error(a.Pos, directive+" needs type=", "example: "+i.Meta().Example)
		return nil, ds
	}
	spec, known := inputTypes[typ.Text]
	if !known || (i.kind == kindArg && spec.argCtor == "") {
		ds.Error(a.ArgPos(typ.Col), fmt.Sprintf("unsupported type %q for %s", typ.Text, directive),
			"supported: "+strings.Join(typeNames(i.kind), ", "))
		return nil, ds
	}
	nd.typ = typ.Text

	if v, ok := args.Attr["help"]; ok {
		nd.help = v.Text
	}
	for _, ba := range []struct {
		key string
		dst *bool
	}{{"required", &nd.required}, {"variadic", &nd.variadic}, {"hidden", &nd.hidden}} {
		v, ok := args.Attr[ba.key]
		if !ok {
			continue
		}
		if v.Text != "true" && v.Text != "false" {
			ds.Error(a.ArgPos(v.Col), fmt.Sprintf("%s= wants true or false (got %q)", ba.key, v.Text), "")
			return nil, ds
		}
		*ba.dst = v.Text == "true"
	}
	if v, ok := args.Attr["short"]; ok {
		r := []rune(v.Text)
		if len(r) != 1 || !(r[0] >= 'a' && r[0] <= 'z' || r[0] >= '0' && r[0] <= '9') {
			ds.Error(a.ArgPos(v.Col), fmt.Sprintf("short= wants exactly one [a-z0-9] rune (got %q)", v.Text), "")
			return nil, ds
		}
		nd.short = r[0]
	}
	if v, ok := args.Attr["env"]; ok {
		nd.env = v.Text
	}
	if v, ok := args.Attr["placeholder"]; ok {
		nd.placeholder = v.Text
	}
	if v, ok := args.Attr["group"]; ok {
		nd.group = v.Text
	}
	if v, ok := args.Attr["default"]; ok {
		if !spec.defaultOK {
			ds.Error(a.ArgPos(v.Col), fmt.Sprintf("default= is not supported for type %q", nd.typ),
				"supported for string, bool, int, int64, float64, duration")
			return nil, ds
		}
		lit, err := spec.canon(v.Text)
		if err != nil {
			ds.Error(a.ArgPos(v.Col), fmt.Sprintf("default=%s does not parse as %s: %v", v.Text, nd.typ, err), "")
			return nil, ds
		}
		nd.def, nd.hasDef = lit, true
	}
	if v, ok := args.Attr["values"]; ok {
		if !spec.valuesOK {
			ds.Error(a.ArgPos(v.Col), fmt.Sprintf("values= is not supported for type %q", nd.typ),
				"supported for scalar string, int, int64, float64")
			return nil, ds
		}
		for _, el := range strings.Split(v.Text, ",") {
			lit, err := spec.canon(el)
			if err != nil {
				ds.Error(a.ArgPos(v.Col), fmt.Sprintf("values= element %q does not parse as %s: %v", el, nd.typ, err), "")
				return nil, ds
			}
			nd.values = append(nd.values, lit)
		}
	}
	if i.kind == kindFlag {
		if nd.name == "help" || nd.short == 'h' {
			ds.Error(a.Pos, "flag name help and short h are reserved by the cli library", "")
			return nil, ds
		}
		if nd.variadic {
			ds.Error(a.Pos, "variadic= applies to arguments, not flags", "")
			return nil, ds
		}
	}
	if i.kind == kindArg && nd.variadic && nd.typ != "strings" {
		ds.Error(a.Pos, fmt.Sprintf("variadic arguments must have type strings (got %q)", nd.typ), "")
		return nil, ds
	}
	if i.kind == kindArg && nd.typ == "strings" && !nd.variadic {
		ds.Error(a.Pos, "arguments of type strings must declare variadic=true",
			"the cli library accepts a slice argument only as the variadic tail")
		return nil, ds
	}
	return nd, ds
}

func (i *Input) Check(n any, t gen.Typed) diag.Diagnostics {
	nd := n.(*inputNode)
	i.fam.inputs[nd.decl] = append(i.fam.inputs[nd.decl], nd)
	i.fam.order = append(i.fam.order, nd)
	return nil
}

func (*Input) Emit(any, *gen.Gen) diag.Diagnostics { return nil }

// Example implements //fabrik:cli:example.
type Example struct{ fam *family }

func (*Example) Name() string { return "cli:example" }

func (*Example) Meta() gen.Meta {
	return gen.Meta{
		Synopsis: "Help example of a CLI command",
		Doc: "**`//fabrik:cli:example cmd= [help=]`**\n\n" +
			"Declared alongside `//fabrik:cli:command`: one entry in the command's " +
			"--help Examples section, in declaration order.\n\n" +
			"```go\n//fabrik:cli:example cmd=\"app migrate up\" help=\"Apply all pending migrations.\"\n```",
		Example: "//fabrik:cli:example cmd=\"app migrate up\"",
		Tier:    gen.TierBind,
		Attrs: []gen.AttrSpec{
			{Key: "cmd", Kind: gen.KindFreeform},
			{Key: "help", Kind: gen.KindFreeform},
		},
	}
}

func (e *Example) Parse(a gen.Annotation) (any, diag.Diagnostics) {
	args, ds := gen.ParseArgs(a, e.Meta())
	if ds.HasFatal() {
		return nil, ds
	}
	cmd, ok := args.Attr["cmd"]
	if !ok {
		ds.Error(a.Pos, "//fabrik:cli:example needs cmd=", "example: "+e.Meta().Example)
		return nil, ds
	}
	nd := &exampleNode{pos: a.Pos, decl: a.Decl, cmd: cmd.Text}
	if v, ok := args.Attr["help"]; ok {
		nd.help = v.Text
	}
	return nd, ds
}

func (e *Example) Check(n any, t gen.Typed) diag.Diagnostics {
	nd := n.(*exampleNode)
	e.fam.examples[nd.decl] = append(e.fam.examples[nd.decl], nd)
	e.fam.exOrder = append(e.fam.exOrder, nd)
	return nil
}

func (*Example) Emit(any, *gen.Gen) diag.Diagnostics { return nil }

func typeNames(k inputKind) []string {
	var out []string
	for name, spec := range inputTypes {
		if k == kindArg && spec.argCtor == "" {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func camelToken(tok string) string {
	var b strings.Builder
	for _, part := range strings.Split(tok, "-") {
		r := []rune(part)
		b.WriteRune(unicode.ToUpper(r[0]))
		b.WriteString(string(r[1:]))
	}
	return b.String()
}

// durationLiteral renders a duration as a sum of named time constants.
func durationLiteral(d time.Duration, timePkg string) string {
	if d == 0 {
		return "0"
	}
	units := []struct {
		name string
		d    time.Duration
	}{
		{"Hour", time.Hour}, {"Minute", time.Minute}, {"Second", time.Second},
		{"Millisecond", time.Millisecond}, {"Microsecond", time.Microsecond}, {"Nanosecond", time.Nanosecond},
	}
	if d == math.MinInt64 {
		// -d overflows; the literal is still expressible in nanoseconds.
		return fmt.Sprintf("-9223372036854775808 * %s.Nanosecond", timePkg)
	}
	neg := d < 0
	if neg {
		d = -d
	}
	var terms []string
	for _, u := range units {
		if n := d / u.d; n > 0 {
			terms = append(terms, fmt.Sprintf("%d*%s.%s", n, timePkg, u.name))
			d -= n * u.d
		}
	}
	out := strings.Join(terms, " + ")
	if neg {
		out = "-(" + out + ")"
	}
	return out
}

// literalExpr renders canonical values after the time import alias is known.
func literalExpr(typ, canonical, timePkg string) string {
	if typ == "duration" {
		d, _ := time.ParseDuration(canonical)
		return durationLiteral(d, timePkg)
	}
	return canonical
}
