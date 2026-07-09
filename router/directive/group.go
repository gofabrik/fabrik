package directive

import (
	"fmt"
	"go/token"
	"go/types"
	"strings"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

// groupInfo is one registered group.
type groupInfo struct {
	prefix string
	refs   []mwRef
	pos    token.Position
	used   bool
}

// Group is the //fabrik:http:group directive.
type Group struct {
	byType map[*types.TypeName]*groupInfo
}

// NewGroup returns a Group directive for one run.
func NewGroup() *Group { return &Group{byType: map[*types.TypeName]*groupInfo{}} }

func (*Group) Name() string { return "http:group" }

func (*Group) Meta() gen.Meta {
	return gen.Meta{
		Synopsis: "Route prefix and shared middleware for a handler struct",
		Doc: "**`//fabrik:http:group /prefix [middleware=name,name2]`**\n\n" +
			"Declared on a handler struct: every `//fabrik:http` route on its " +
			"methods is registered under the prefix, wrapped in the group's " +
			"middleware before its own. A route path of `/{$}` maps to the " +
			"bare prefix. Plain-function routes are unaffected.\n\n" +
			"```go\n//fabrik:http:group /api middleware=auth\ntype API struct { ... }\n```",
		Example: "//fabrik:http:group /api",
		Pos: []gen.PosSpec{
			{Name: "PREFIX", Kind: gen.KindFreeform},
		},
		Attrs: []gen.AttrSpec{
			{Key: "middleware", Kind: gen.KindMiddlewareRef},
		},
	}
}

type groupNode struct {
	prefix string
	refs   []mwRef
	pos    token.Position
}

func (gr *Group) Parse(a gen.Annotation) (any, diag.Diagnostics) {
	args, ds := gen.ParseArgs(a, gr.Meta())
	if len(args.Pos) < 1 {
		return nil, ds
	}
	prefix := args.Pos[0]
	switch {
	case !strings.HasPrefix(prefix.Text, "/") || prefix.Text == "/":
		ds.Error(a.ArgPos(prefix.Col), fmt.Sprintf("group prefix must be a non-root path starting with %q (got %q)", "/", prefix.Text),
			"example: //fabrik:http:group /api")
	case strings.Contains(prefix.Text, "{$}") || strings.Contains(prefix.Text, "...}"):
		ds.Error(a.ArgPos(prefix.Col), fmt.Sprintf("group prefix cannot contain %q or %q (got %q)", "{$}", "{name...}", prefix.Text),
			"terminal wildcards cannot have routes appended after them")
	default:
		if pe := patternError(prefix.Text); pe != "" {
			ds.Error(a.ArgPos(prefix.Col), "invalid group prefix: "+pe,
				"wildcards: /{name}")
		}
	}
	nd := &groupNode{prefix: strings.TrimSuffix(prefix.Text, "/"), pos: a.Pos}
	if mw, ok := args.Attr["middleware"]; ok {
		refs, rds := parseMWRefs(a, mw)
		ds = append(ds, rds...)
		nd.refs = refs
	}
	if ds.HasFatal() {
		return nil, ds
	}
	return nd, ds
}

func (gr *Group) Check(n any, t gen.Typed) diag.Diagnostics {
	nd := n.(*groupNode)
	var ds diag.Diagnostics

	tn, ok := t.Target.(*types.TypeName)
	if !ok {
		ds.Error(nd.pos, "//fabrik:http:group must be on a struct type declaration", "")
		return ds
	}
	if !isNamedStruct(tn.Type()) {
		ds.Error(nd.pos, fmt.Sprintf("//fabrik:http:group must be on a struct type (%s is not a struct)", tn.Name()),
			"handler structs hold the group's routes as methods")
		return ds
	}
	if namedOf(tn.Type()).TypeParams().Len() > 0 {
		ds.Error(nd.pos, fmt.Sprintf("//fabrik:http:group cannot be on a generic type (%s has type parameters)", tn.Name()),
			"declare a concrete struct")
		return ds
	}
	if first, dup := gr.byType[tn]; dup {
		ds.Error(nd.pos, fmt.Sprintf("duplicate group on %s", tn.Name()),
			fmt.Sprintf("first declared at %s", first.pos))
		return ds
	}

	gr.byType[tn] = &groupInfo{prefix: nd.prefix, refs: nd.refs, pos: nd.pos}
	return ds
}

func (gr *Group) Emit(any, *gen.Gen) diag.Diagnostics { return nil }

// Validate warns about groups without routed methods.
func (gr *Group) Validate(*gen.Gen) diag.Diagnostics {
	var ds diag.Diagnostics
	for tn, info := range gr.byType {
		if !info.used {
			ds.Warn(info.pos, fmt.Sprintf("group on %s has no routes", tn.Name()),
				"routes are methods of the struct annotated with //fabrik:http")
		}
	}
	return ds
}

// lookup returns the group registered for the receiver type.
func (gr *Group) lookup(tn *types.TypeName) *groupInfo {
	info := gr.byType[tn]
	if info != nil {
		info.used = true
	}
	return info
}
