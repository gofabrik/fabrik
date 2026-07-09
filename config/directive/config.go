// Package directive provides the //fabrik:config codegen directive.
package directive

import (
	"fmt"
	"go/token"
	"go/types"
	"reflect"
	"sort"
	"strings"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

const configPath = "github.com/gofabrik/fabrik/config"

// Config loads annotated config structs and binds them into DI.
type Config struct {
	files     []string         // YAML layers, in override order
	byType    map[string]*Node // TypeString of *T -> node
	bySection map[string]*Node
	nodes     []*Node
}

// New returns the config directive for one generation run.
func New(files ...string) *Config {
	return &Config{files: files, byType: map[string]*Node{}, bySection: map[string]*Node{}}
}

func (*Config) Name() string { return "config" }

func (*Config) Meta() gen.Meta {
	return gen.Meta{
		Synopsis: "Configuration struct: [section]",
		Doc: "**`//fabrik:config [section]`**\n\n" +
			"Marks a struct as configuration: generated code loads it in the " +
			"Config phase (before inits and providers) with `config.Load` and " +
			"binds the pointer into DI. Both conventional files (config.yaml, " +
			"config.local.yaml) are optional: a missing file means defaults " +
			"and env overrides apply. `yaml:` tags name keys, `default:` " +
			"tags set fallbacks, `env:\"NAME\"` opts a field into environment " +
			"override. The optional section scopes a domain package to its " +
			"subtree of config.yaml (`store` -> keys under `store:`); the name " +
			"is a single key, no dots. Structs " +
			"sharing the file must each declare a section; a single " +
			"sectionless struct owns the whole file and cannot be combined " +
			"with sectioned ones. When every struct is sectioned, " +
			"generated Load calls validate the file's top-level keys against " +
			"the declared sections, so a typo'd section fails startup.\n\n" +
			"```go\n//fabrik:config store\ntype Config struct {\n\tKind string `yaml:\"kind\" default:\"memory\"`\n}\n```",
		Example: "//fabrik:config store",
		Pos: []gen.PosSpec{
			{Name: "SECTION", Kind: gen.KindFreeform, Optional: true},
		},
		Tier: gen.TierBind,
	}
}

// Node is one registered config struct.
type Node struct {
	section string
	pos     token.Position

	named      *types.Named
	fset       *token.FileSet
	built      bool // a Load call was emitted
	referenced bool // suppresses unused warnings after related diagnostics
}

// Named returns the config struct's named type.
func (n *Node) Named() *types.Named { return n.named }

func (c *Config) Parse(a gen.Annotation) (any, diag.Diagnostics) {
	args, ds := gen.ParseArgs(a, c.Meta())
	nd := &Node{pos: a.Pos}
	if len(args.Pos) > 0 {
		nd.section = args.Pos[0].Text
	}
	if strings.Contains(nd.section, ".") {
		ds.Error(a.Pos, fmt.Sprintf("section name %q must not contain dots", nd.section),
			"a section is a single top-level key; model nesting as struct fields and address them as <section>.<field>")
	}
	if ds.HasFatal() {
		return nil, ds
	}
	return nd, ds
}

func (c *Config) Check(n any, t gen.Typed) diag.Diagnostics {
	nd := n.(*Node)
	var ds diag.Diagnostics

	tn, ok := t.Target.(*types.TypeName)
	if !ok {
		ds.Error(nd.pos, "//fabrik:config must be on a struct type declaration", "")
		return ds
	}
	named, ok := types.Unalias(tn.Type()).(*types.Named)
	if !ok {
		ds.Error(nd.pos, "//fabrik:config must be on a struct type declaration", "")
		return ds
	}
	if _, ok := named.Underlying().(*types.Struct); !ok {
		ds.Error(nd.pos, fmt.Sprintf("//fabrik:config must be on a struct type (%s is not a struct)", tn.Name()),
			"configuration is a struct with yaml tags")
		return ds
	}
	if named.TypeParams().Len() > 0 {
		ds.Error(nd.pos, fmt.Sprintf("//fabrik:config cannot be on a generic type (%s has type parameters)", tn.Name()),
			"configuration is loaded by reflection at startup; declare a concrete struct")
		return ds
	}

	key := types.TypeString(types.NewPointer(named), nil)
	if first, dup := c.byType[key]; dup {
		ds.Error(nd.pos, fmt.Sprintf("duplicate //fabrik:config on %s", tn.Name()),
			fmt.Sprintf("first declared at %s", first.pos))
		return ds
	}
	if first, dup := c.bySection[nd.section]; dup {
		ds.Error(nd.pos, fmt.Sprintf("section %q is already configured by %s", nd.section, first.named.Obj().Name()),
			fmt.Sprintf("first declared at %s", first.pos))
		return ds
	}
	if first, ok := c.bySection[""]; ok && nd.section != "" {
		ds.Error(nd.pos, fmt.Sprintf("sectionless config struct %s owns the whole file and cannot be combined with sectioned config structs", first.named.Obj().Name()),
			fmt.Sprintf("sectionless struct declared at %s; declare a section for every config struct", first.pos))
		return ds
	}
	if nd.section == "" && len(c.nodes) > 0 {
		ds.Error(nd.pos, fmt.Sprintf("sectionless config struct %s owns the whole file and cannot be combined with sectioned config structs", tn.Name()),
			"declare a section for every config struct")
		return ds
	}
	nd.named = named
	nd.fset = t.Fset
	c.byType[key] = nd
	c.bySection[nd.section] = nd
	c.nodes = append(c.nodes, nd)
	return ds
}

// Emit binds the config struct lazily.
func (c *Config) Emit(n any, g *gen.Gen) diag.Diagnostics {
	nd := n.(*Node)
	if nd.named == nil {
		return nil
	}
	ptr := types.NewPointer(nd.named)
	g.BindLazy(ptr, "", func() (string, diag.Diagnostics) {
		v, stmt := c.LoadStmt(nd, g)
		g.Stmt(gen.PhaseConfig, "%s", stmt)
		return v, nil
	})
	return nil
}

// LoadStmt renders the config.Load statement for a node.
func (c *Config) LoadStmt(nd *Node, g *gen.Gen) (string, string) {
	nd.built = true
	cfgPkg := g.Import(configPath)
	opts := make([]string, 0, len(c.files)+1)
	for _, f := range c.files {
		opts = append(opts, fmt.Sprintf("%s.FileOptional(%q)", cfgPkg, f))
	}
	if known := c.knownSections(); known != nil {
		quoted := make([]string, len(known))
		for i, k := range known {
			quoted[i] = fmt.Sprintf("%q", k)
		}
		opts = append(opts, fmt.Sprintf("%s.KnownSections(%s)", cfgPkg, strings.Join(quoted, ", ")))
	}
	if nd.section != "" {
		opts = append(opts, fmt.Sprintf("%s.Section(%q)", cfgPkg, nd.section))
	}
	v := g.Var(nd.named.Obj().Pkg().Name() + nd.named.Obj().Name())
	stmt := fmt.Sprintf("%s, err := %s.Load[%s](%s)\nif err != nil {\nreturn err\n}",
		v, cfgPkg, g.TypeExpr(nd.named), strings.Join(opts, ", "))
	return v, stmt
}

// NodeFor returns the config node for a *Config parameter type, or nil.
func (c *Config) NodeFor(t types.Type) *Node {
	return c.byType[types.TypeString(types.Unalias(t), nil)]
}

// Validate warns about unused config structs.
func (c *Config) Validate(*gen.Gen) diag.Diagnostics {
	var ds diag.Diagnostics
	for _, nd := range c.nodes {
		if nd.named != nil && !nd.built && !nd.referenced {
			ds.Warn(nd.pos, fmt.Sprintf("config struct %s is never used", nd.named.Obj().Name()),
				"inject it into a provider, init, or handler struct, or remove the directive")
		}
	}
	return ds
}

// IsConfig reports whether t is a registered *Config type.
func (c *Config) IsConfig(t types.Type) bool {
	_, ok := c.byType[types.TypeString(types.Unalias(t), nil)]
	return ok
}

// MissingHint explains config structs passed by value and marks them referenced.
func (c *Config) MissingHint(t types.Type) (string, bool) {
	nd := c.NodeFor(types.NewPointer(types.Unalias(t)))
	if nd == nil {
		return "", false
	}
	nd.referenced = true
	name := types.TypeString(types.Unalias(t), func(p *types.Package) string { return p.Name() })
	return "config structs are injected as pointers; take *" + name, true
}

// Key is a resolved config field.
type Key struct {
	Path    []string
	Type    types.Type
	Default string
}

// ResolveKey maps a dotted config key to its struct field.
func (c *Config) ResolveKey(key string) (*Node, Key, bool) {
	segs := strings.Split(key, ".")
	if nd, ok := c.bySection[segs[0]]; ok && len(segs) > 1 {
		if kf, ok := fieldPath(nd.named, segs[1:]); ok {
			return nd, kf, true
		}
	}
	if nd, ok := c.bySection[""]; ok {
		if kf, ok := fieldPath(nd.named, segs); ok {
			return nd, kf, true
		}
	}
	return nil, Key{}, false
}

// knownSections returns sorted section names for generated validation.
func (c *Config) knownSections() []string {
	if _, sectionless := c.bySection[""]; sectionless {
		return nil
	}
	names := make([]string, 0, len(c.bySection))
	for name := range c.bySection {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// fieldPath follows yaml tag names, including inline fields and yaml:"-" exclusions.
func fieldPath(named *types.Named, segs []string) (Key, bool) {
	st, ok := named.Underlying().(*types.Struct)
	if !ok {
		return Key{}, false
	}
	return structFieldPath(st, segs)
}

func structFieldPath(st *types.Struct, segs []string) (Key, bool) {
	for j := 0; j < st.NumFields(); j++ {
		f := st.Field(j)
		if !f.Exported() {
			continue
		}
		if ist, ok := types.Unalias(f.Type()).Underlying().(*types.Struct); ok && yamlInlineTag(st.Tag(j)) {
			if kf, ok := structFieldPath(ist, segs); ok {
				kf.Path = append([]string{f.Name()}, kf.Path...)
				return kf, true
			}
			continue
		}
		name := yamlFieldName(f, st.Tag(j))
		if name == "-" || name != segs[0] {
			continue
		}
		if len(segs) == 1 {
			def, _ := reflect.StructTag(st.Tag(j)).Lookup("default")
			return Key{Path: []string{f.Name()}, Type: f.Type(), Default: def}, true
		}
		nst, ok := types.Unalias(f.Type()).Underlying().(*types.Struct)
		if !ok {
			return Key{}, false
		}
		if kf, ok := structFieldPath(nst, segs[1:]); ok {
			kf.Path = append([]string{f.Name()}, kf.Path...)
			return kf, true
		}
		return Key{}, false
	}
	return Key{}, false
}

// yamlInlineTag reports whether a struct tag carries yaml's ",inline".
func yamlInlineTag(tag string) bool {
	_, opts, _ := strings.Cut(reflect.StructTag(tag).Get("yaml"), ",")
	for _, o := range strings.Split(opts, ",") {
		if o == "inline" {
			return true
		}
	}
	return false
}

// yamlFieldName returns the config library's YAML field name.
func yamlFieldName(f *types.Var, tag string) string {
	name, _, _ := strings.Cut(reflect.StructTag(tag).Get("yaml"), ",")
	if name == "" {
		return strings.ToLower(f.Name())
	}
	return name
}
