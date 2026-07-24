package gen

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// renderNode renders one node to source lines.
func renderNode(n Node) []string {
	switch n := n.(type) {
	case *Raw:
		return n.Lines
	case *Assign:
		return []string{n.Var + " := " + n.Expr}
	case *Call:
		if n.Cleanup != "" {
			return renderCleanupCall(n)
		}
		return renderCall(n.Var, n.Fn, n.Args, n.Err)
	case *ConfigLoad:
		opening, closing := "(", ")"
		if n.Prefix != "" {
			opening, closing = "(append("+n.Prefix+",", ")...)"
		}
		lines := []string{fmt.Sprintf("%s, err := %s.Load[%s]%s", n.Var, n.Pkg, n.Type, opening)}
		for _, opt := range n.Options {
			lines = append(lines, opt+",")
		}
		lines = append(lines, closing)
		return append(lines, errReturn()...)
	case *StructLit:
		if len(n.Fields) == 0 {
			return []string{fmt.Sprintf("%s := &%s{}", n.Var, n.Type)}
		}
		lines := []string{fmt.Sprintf("%s := &%s{", n.Var, n.Type)}
		for _, f := range n.Fields {
			lines = append(lines, f.Name+": "+f.Expr+",")
		}
		return append(lines, "}")
	case *Select:
		return renderSelect(n)
	case *Route:
		return renderRoute(n)
	case *Serve:
		return []string{"return " + n.Expr}
	}
	panic("gen: unrenderable node kind")
}

func renderCall(v, fn string, args []string, errStyle ErrStyle) []string {
	call := fn + "(" + strings.Join(args, ", ") + ")"
	switch errStyle {
	case ErrReturn:
		return append([]string{v + ", err := " + call}, errReturn()...)
	case ErrInline:
		return []string{"if err := " + call + "; err != nil {", "return err", "}"}
	}
	if v == "" {
		return []string{call}
	}
	return []string{v + " := " + call}
}

// renderCleanupCall emits a nil-guarded deferred cleanup.
func renderCleanupCall(n *Call) []string {
	call := n.Fn + "(" + strings.Join(n.Args, ", ") + ")"
	var lines []string
	if n.Err == ErrReturn {
		lines = append([]string{n.Var + ", " + n.Cleanup + ", err := " + call}, errReturn()...)
	} else {
		lines = []string{n.Var + ", " + n.Cleanup + " := " + call}
	}
	return append(lines, "if "+n.Cleanup+" != nil {", "defer "+n.Cleanup+"()", "}")
}

func errReturn() []string {
	return []string{"if err != nil {", "return err", "}"}
}

func renderSelect(n *Select) []string {
	lines := []string{
		"var " + n.Var + " " + n.Iface,
		"switch " + n.KeyExpr + " {",
	}
	for _, c := range n.Cases {
		lines = append(lines, "case "+strconv.Quote(c.Value)+":")
		for _, b := range c.Body {
			lines = append(lines, renderNode(b)...)
		}
		call := c.Result.Fn + "(" + strings.Join(c.Result.Args, ", ") + ")"
		if c.Result.Err == ErrReturn {
			lines = append(lines, c.Result.Var+", err := "+call)
			lines = append(lines, errReturn()...)
			lines = append(lines, n.Var+" = "+c.Result.Var)
		} else {
			lines = append(lines, n.Var+" = "+call)
		}
	}
	lines = append(lines,
		"default:",
		fmt.Sprintf("return %s.Errorf(\"no %s implementation for %%q\", %s)", n.FmtPkg, n.Iface, n.KeyExpr),
		"}")
	return lines
}

func renderRoute(n *Route) []string {
	switch n.Kind {
	case RouteMethod:
		args := strconv.Quote(n.Method) + ", " + strconv.Quote(n.Pattern) + ", " + n.Handler
		if len(n.Chain) > 0 {
			args += ", " + strings.Join(n.Chain, ", ")
		}
		return []string{n.Router + ".Method(" + args + ")"}
	case RouteHandleFunc:
		return []string{n.Router + ".HandleFunc(" + strconv.Quote(n.Pattern) + ", " + n.Handler + ")"}
	default:
		expr := n.Handler
		for i := len(n.Chain) - 1; i >= 0; i-- {
			expr = n.Chain[i] + "(" + expr + ")"
		}
		return []string{n.Router + ".Handle(" + strconv.Quote(n.Pattern) + ", " + expr + ")"}
	}
}

// phaseNode pairs a node with its emission index.
type phaseNode struct {
	n    Node
	emit int
}

// anchorLess orders nodes by source position, then emission index.
func anchorLess(a, b phaseNode) bool {
	ap, bp := a.n.base().Origin.Pos, b.n.base().Origin.Pos
	if ap.IsValid() != bp.IsValid() {
		return ap.IsValid()
	}
	if ap.Filename != bp.Filename {
		return ap.Filename < bp.Filename
	}
	if ap.Line != bp.Line {
		return ap.Line < bp.Line
	}
	if ap.Column != bp.Column {
		return ap.Column < bp.Column
	}
	return a.emit < b.emit
}

// layoutPhase keeps dependent nodes together and orders independent clusters by anchor.
func layoutPhase(nodes []phaseNode) [][]phaseNode {
	owner := map[string]int{}
	for i, pn := range nodes {
		for _, d := range defines(pn.n) {
			owner[d] = i
		}
	}
	vars := make(map[string]bool, len(owner))
	for v := range owner {
		vars[v] = true
	}

	deps := make([][]int, len(nodes)) // deps[i] = nodes that must precede i
	comp := make([]int, len(nodes))   // union-find parent
	for i := range comp {
		comp[i] = i
	}
	find := func(x int) int {
		for comp[x] != x {
			comp[x] = comp[comp[x]]
			x = comp[x]
		}
		return x
	}

	for i, pn := range nodes {
		for _, u := range uses(pn.n, vars) {
			j := owner[u]
			if j == i {
				continue
			}
			deps[i] = append(deps[i], j)
			comp[find(i)] = find(j)
		}
	}

	groups := map[int][]int{}
	for i := range nodes {
		root := find(i)
		groups[root] = append(groups[root], i)
	}
	roots := make([]int, 0, len(groups))
	for root := range groups {
		roots = append(roots, root)
	}
	sort.Slice(roots, func(a, b int) bool {
		return anchorLess(earliest(nodes, groups[roots[a]]), earliest(nodes, groups[roots[b]]))
	})

	var clusters [][]phaseNode
	for _, root := range roots {
		clusters = append(clusters, orderCluster(nodes, deps, groups[root]))
	}
	return clusters
}

// earliest returns the member with the smallest anchor.
func earliest(nodes []phaseNode, members []int) phaseNode {
	best := nodes[members[0]]
	for _, i := range members[1:] {
		if anchorLess(nodes[i], best) {
			best = nodes[i]
		}
	}
	return best
}

// orderCluster emits producers before consumers.
func orderCluster(nodes []phaseNode, deps [][]int, members []int) []phaseNode {
	pending := map[int]bool{}
	for _, i := range members {
		pending[i] = true
	}
	var out []phaseNode
	for len(pending) > 0 {
		best := -1
		for _, i := range members {
			if !pending[i] {
				continue
			}
			ready := true
			for _, j := range deps[i] {
				if pending[j] {
					ready = false
					break
				}
			}
			if !ready {
				continue
			}
			if best < 0 || anchorLess(nodes[i], nodes[best]) {
				best = i
			}
		}
		if best < 0 { // dependency cycle: fall back to emission order
			for _, i := range members {
				if pending[i] {
					best = i
					break
				}
			}
		}
		delete(pending, best)
		out = append(out, nodes[best])
	}
	return out
}

// spacedCluster reports whether a cluster needs surrounding blank lines.
func spacedCluster(cluster []phaseNode) bool {
	for _, pn := range cluster {
		if pn.n.base().Label != "" || len(renderNode(pn.n)) > 1 {
			return true
		}
	}
	return false
}
