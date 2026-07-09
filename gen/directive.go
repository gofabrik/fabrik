// Package gen defines the directive contract and generated-code builder.
package gen

import (
	"go/ast"
	"go/token"
	"go/types"

	"github.com/gofabrik/fabrik/diag"
)

// Annotation is the syntactic view of one directive comment.
type Annotation struct {
	Name    string         // keyword after //fabrik:
	Args    string         // raw text after the keyword
	Pos     token.Position // position of the directive comment
	ArgsCol int            // column where Args starts on the directive line
	Decl    ast.Node       // annotated declaration: *ast.FuncDecl or *ast.TypeSpec
}

// ArgPos returns the source position for an offset within Args.
func (a Annotation) ArgPos(col int) token.Position {
	p := a.Pos
	p.Column = a.ArgsCol + col
	return p
}

// Typed is the semantic view of an annotation.
type Typed struct {
	Target types.Object   // the annotated func/method/type, fully typed
	Fset   *token.FileSet // resolves positions of typed objects
}

// Directive implements one //fabrik:NAME annotation.
type Directive interface {
	Name() string
	Meta() Meta
	Parse(Annotation) (any, diag.Diagnostics)
	Check(node any, t Typed) diag.Diagnostics
	Emit(node any, g *Gen) diag.Diagnostics
}

// Parsed pairs a node with the directive that produced it.
type Parsed struct {
	Directive Directive
	Node      any
}

// Finisher runs after every directive has emitted.
type Finisher interface {
	Finish(g *Gen) diag.Diagnostics
}

// NodePreparer registers bindings before dependency resolution.
type NodePreparer interface {
	PrepareNode(node any, g *Gen)
}

// Meta describes directive syntax, docs, and completions.
type Meta struct {
	Synopsis string     // one line, shown as completion detail
	Doc      string     // markdown, shown on hover
	Example  string     // canonical usage, shown in diagnostics help
	Pos      []PosSpec  // required positional arguments, in order
	Attrs    []AttrSpec // key=value options
}

// PosSpec describes one positional argument. Optional positionals must
// trail the required ones.
type PosSpec struct {
	Name     string    // e.g. "METHOD"; used in messages and docs
	Kind     ValueKind // completion hint; values are validated by Parse
	Values   []string  // completion candidates; a closed set only when Kind is KindEnum
	Optional bool
}

// AttrSpec describes one key=value option.
type AttrSpec struct {
	Key      string
	Kind     ValueKind
	Values   []string // completion candidates; a closed set only when Kind is KindEnum
	Required bool
}

// ValueKind classifies completion behavior for argument values.
type ValueKind int

const (
	KindFreeform ValueKind = iota
	KindEnum
	KindMiddlewareRef // completes from middleware-shaped functions in the app
)
