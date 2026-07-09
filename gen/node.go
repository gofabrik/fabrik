package gen

import "go/token"

// Origin ties a node to the directive occurrence that produced it.
type Origin struct {
	Directive string
	Pos       token.Position
}

// Base is shared node metadata.
type Base struct {
	Origin Origin
	Phase  Phase    // run() section; child nodes inherit their parent's phase
	Label  string   // optional one-line comment above the node
	Uses   []string // manual dependency additions; rendered text supplies the rest
}

func (b *Base) base() *Base { return b }

// Node is one emission in the generation tree.
type Node interface {
	base() *Base
}

// Raw is a preformatted emission.
type Raw struct {
	Base
	Lines   []string
	Defines []string // variables the lines declare, for dependency ordering
}

// Assign declares a variable from an expression: `v := expr`.
type Assign struct {
	Base
	Var  string
	Expr string
}

// ErrStyle says how a call's error result is handled.
type ErrStyle int

const (
	ErrNone   ErrStyle = iota // no error result
	ErrReturn                 // v, err := call; if err != nil { return err }
	ErrInline                 // if err := call; err != nil { return err }
)

// Call invokes a constructor or registration function.
type Call struct {
	Base
	Var  string // assigned variable; "" for a bare call
	Fn   string // rendered callee, e.g. "shared.InitLogger" or "r.Use"
	Args []string
	Err  ErrStyle
}

// ConfigLoad loads one configuration struct.
type ConfigLoad struct {
	Base
	Var     string
	Pkg     string   // config package alias
	Type    string   // rendered struct type
	Options []string // rendered option expressions
}

// StructLit constructs a pointer to a struct with injected fields.
type StructLit struct {
	Base
	Var    string
	Type   string // rendered struct type, without the &
	Fields []Field
}

// Field is one injected struct field.
type Field struct {
	Name, Expr string
}

// Select wires the implementation a configuration value names.
type Select struct {
	Base
	Var     string // the interface variable being assigned
	Iface   string // rendered interface type
	KeyExpr string // e.g. "webConfig.Kind"
	FmtPkg  string // fmt alias, for the unmatched-value arm
	Cases   []Case
}

// Case is one selection arm: branch-local work, then the constructor.
type Case struct {
	Value  string
	Body   []Node // e.g. branch-local config loads
	Result Call   // Var set when the constructor returns an error
}

// RouteKind distinguishes route registration forms.
type RouteKind int

const (
	RouteMethod     RouteKind = iota // Router.Method(m, p, handler, chain...)
	RouteHandle                      // Router.Handle(p, chain-wrapped handler)
	RouteHandleFunc                  // Router.HandleFunc(p, handler)
)

// Route registers one route on the router.
type Route struct {
	Base
	Router  string
	Kind    RouteKind
	Method  string
	Pattern string
	Handler string   // rendered handler expression
	Chain   []string // middleware expressions, outermost first
}

// Serve returns from run with the serving call.
type Serve struct {
	Base
	Expr string
}

// Node appends n and fills a missing directive origin.
func (g *Gen) Node(n Node) {
	b := n.base()
	if b.Origin.Directive == "" {
		b.Origin.Directive = g.current
	}
	g.nodes = append(g.nodes, n)
}
