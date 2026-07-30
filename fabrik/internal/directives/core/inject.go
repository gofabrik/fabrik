package core

import (
	"fmt"
	"go/token"
	"go/types"
	"strings"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

// Inject is the //fabrik:inject directive.
type Inject struct {
	nodes []*injectNode
	seen  map[types.Object]map[string]token.Position
}

// NewInject returns an Inject directive for one run.
func NewInject() *Inject {
	return &Inject{seen: map[types.Object]map[string]token.Position{}}
}

func (*Inject) Name() string { return "inject" }

func (*Inject) Meta() gen.Meta {
	return gen.Meta{
		Synopsis: "Name the provider for one parameter or field",
		Doc: "**`//fabrik:inject <param-or-field> name=<provider>`**\n\n" +
			"Maps a function parameter or exported struct field to a named " +
			"provider. Stack one directive per mapping in the declaration's doc " +
			"comment. Generated wiring resolves mapped dependencies from the " +
			"provider of the same type with the matching `name=`; unmapped " +
			"dependencies use unnamed providers. Unknown selectors report the " +
			"available parameters or exported fields. Unused mappings are " +
			"errors.\n\n" +
			"Provider-constructed types do not accept field mappings; map the " +
			"provider's parameters instead. CLI value parameters are also " +
			"invalid targets because flags and arguments supply them.\n\n" +
			"```go\n" +
			"//fabrik:provider\n" +
			"func NewDB(cfg *Config) *sql.DB { ... }\n\n" +
			"//fabrik:provider name=replica\n" +
			"func NewReplicaDB(cfg *Config) *sql.DB { ... }\n\n" +
			"type Databases struct {\n" +
			"\tPrimary *sql.DB\n" +
			"\tReplica *sql.DB\n" +
			"}\n\n" +
			"//fabrik:inject replica name=replica\n" +
			"//fabrik:provider\n" +
			"func NewDatabases(primary, replica *sql.DB) *Databases {\n" +
			"\treturn &Databases{Primary: primary, Replica: replica}\n" +
			"}\n" +
			"```",
		Example: "//fabrik:inject db name=replica",
		Pos: []gen.PosSpec{
			{Name: "PARAM-OR-FIELD", Kind: gen.KindFreeform},
		},
		Attrs: []gen.AttrSpec{
			{Key: "name", Kind: gen.KindFreeform, Required: true},
		},
		Tier: gen.TierBind,
	}
}

type injectNode struct {
	pos      token.Position
	selector string
	provName string
	obj      types.Object
}

func (i *Inject) Parse(a gen.Annotation) (any, diag.Diagnostics) {
	args, ds := gen.ParseArgs(a, i.Meta())
	if len(args.Pos) < 1 {
		return nil, ds
	}
	sel := args.Pos[0]
	nd := &injectNode{pos: a.Pos, selector: sel.Text}
	if sel.Text == "_" {
		ds.Error(a.ArgPos(sel.Col), "inject selector must not be the blank identifier",
			"name a parameter or an exported field")
	}
	if nameA, ok := args.Attr["name"]; ok {
		nd.provName = nameA.Text
		switch {
		case nd.provName == "":
			ds.Error(a.ArgPos(nameA.Col), "name= must not be empty",
				"example: //fabrik:inject db name=replica")
		case !nameRE.MatchString(nd.provName):
			ds.Error(a.ArgPos(nameA.Col), fmt.Sprintf("provider name %q is invalid", nd.provName),
				"names are a lowercase letter followed by lowercase letters or digits: [a-z][a-z0-9]*")
		}
	}
	if ds.HasFatal() {
		return nil, ds
	}
	return nd, ds
}

func (i *Inject) Check(n any, t gen.Typed) diag.Diagnostics {
	nd := n.(*injectNode)
	var ds diag.Diagnostics
	switch obj := t.Target.(type) {
	case *types.Func:
		sig := obj.Type().(*types.Signature)
		var names []string
		found := false
		for p := range sig.Params().Len() {
			name := sig.Params().At(p).Name()
			if name == nd.selector {
				found = true
			}
			if name != "" && name != "_" {
				names = append(names, name)
			}
		}
		if !found {
			ds.Error(nd.pos, fmt.Sprintf("unknown parameter %q", nd.selector),
				"known: "+strings.Join(names, ", "))
			return ds
		}
	case *types.TypeName:
		st, ok := types.Unalias(obj.Type()).Underlying().(*types.Struct)
		if !ok {
			ds.Error(nd.pos, "//fabrik:inject must be on a function or struct type declaration", "")
			return ds
		}
		var names []string
		var field *types.Var
		for f := range st.NumFields() {
			fv := st.Field(f)
			if fv.Name() == nd.selector {
				field = fv
			}
			if fv.Exported() {
				names = append(names, fv.Name())
			}
		}
		if field == nil {
			ds.Error(nd.pos, fmt.Sprintf("unknown field %q", nd.selector),
				"known: "+strings.Join(names, ", "))
			return ds
		}
		if !field.Exported() {
			ds.Error(nd.pos, fmt.Sprintf("field %s of %s is unexported", field.Name(), obj.Name()),
				"inject selectors must name an exported field")
			return ds
		}
	default:
		ds.Error(nd.pos, "//fabrik:inject must be on a function or struct type declaration", "")
		return ds
	}
	if first, dup := i.seen[t.Target][nd.selector]; dup {
		ds.Error(nd.pos, fmt.Sprintf("duplicate //fabrik:inject for %q", nd.selector),
			fmt.Sprintf("first declared at %s", first))
		return ds
	}
	if i.seen[t.Target] == nil {
		i.seen[t.Target] = map[string]token.Position{}
	}
	i.seen[t.Target][nd.selector] = nd.pos
	nd.obj = t.Target
	i.nodes = append(i.nodes, nd)
	return ds
}

func (*Inject) Emit(any, *gen.Gen) diag.Diagnostics { return nil }

// Mappings returns accepted declaration mappings.
func (i *Inject) Mappings() map[types.Object]map[string]string {
	table := map[types.Object]map[string]string{}
	for _, nd := range i.nodes {
		if table[nd.obj] == nil {
			table[nd.obj] = map[string]string{}
		}
		table[nd.obj][nd.selector] = nd.provName
	}
	return table
}

// Validate rejects mappings that generated wiring cannot use.
func (i *Inject) Validate(g *gen.Gen) diag.Diagnostics {
	var ds diag.Diagnostics
	for _, nd := range i.nodes {
		if tn, ok := nd.obj.(*types.TypeName); ok && providerOwned(g, tn.Type()) {
			ds.Error(nd.pos, fmt.Sprintf("type %s is constructed by its provider",
				types.TypeString(types.Unalias(tn.Type()), nil)),
				"a provided value receives no field injection; put //fabrik:inject on the provider's parameters instead")
			g.RejectInject(nd.obj, nd.selector)
			continue
		}
		if g.InjectPending(nd.obj, nd.selector) {
			ds.Error(nd.pos, fmt.Sprintf("//fabrik:inject %s has no effect on this declaration", nd.selector),
				"no generated wiring resolves this declaration by name")
		}
	}
	return ds
}

func providerOwned(g *gen.Gen, t types.Type) bool {
	t = types.Unalias(t)
	for _, c := range []types.Type{t, types.NewPointer(t)} {
		for _, name := range g.BindingNames(c) {
			if owner, ok := g.BindingOwner(c, name); ok && owner == "provider" {
				return true
			}
		}
	}
	return false
}
