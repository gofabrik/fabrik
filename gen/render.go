package gen

import (
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// errCtx carries return arity and cleanup unwinding; nil selects the default flow's named error.
type errCtx struct {
	zeros  string
	unwind bool
}

// check renders conditionals with the enclosing return arity and unwind path.
func (ec *errCtx) check(cond string) []string {
	lines := []string{"if " + cond + " {"}
	if ec == nil {
		return append(lines, "return err", "}")
	}
	if ec.unwind {
		return append(lines, "return "+ec.zeros+"unwind(err)", "}")
	}
	return append(lines, "return "+ec.zeros+"err", "}")
}

func (ec *errCtx) errReturn() []string {
	return ec.check("err != nil")
}

func (ec *errCtx) errorExprReturn(expr string) []string {
	if ec == nil {
		return []string{"return " + expr}
	}
	if ec.unwind {
		return []string{"return " + ec.zeros + "unwind(" + expr + ")"}
	}
	return []string{"return " + ec.zeros + expr}
}

func renderNode(n Node, ec *errCtx) []string {
	switch n := n.(type) {
	case *Raw:
		if n.Check {
			return append(append([]string{}, n.Lines...), ec.errReturn()...)
		}
		return n.Lines
	case *Assign:
		return []string{n.Var + " := " + n.Expr}
	case *Call:
		if n.Cleanup != "" {
			return renderCleanupCall(n, ec)
		}
		return renderCall(n.Var, n.Fn, n.Args, n.Err, ec)
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
		return append(lines, ec.errReturn()...)
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
		return renderSelect(n, ec)
	case *Route:
		return renderRoute(n)
	}
	panic("gen: unrenderable node kind")
}

func renderCall(v, fn string, args []string, errStyle ErrStyle, ec *errCtx) []string {
	call := fn + "(" + strings.Join(args, ", ") + ")"
	switch errStyle {
	case ErrReturn:
		return append([]string{v + ", err := " + call}, ec.errReturn()...)
	case ErrInline:
		return ec.check("err := " + call + "; err != nil")
	}
	if v == "" {
		return []string{call}
	}
	return []string{v + " := " + call}
}

// renderCleanupCall defers error joining in the default flow but leaves scoped cleanup to unwind paths.
func renderCleanupCall(n *Call, ec *errCtx) []string {
	call := n.Fn + "(" + strings.Join(n.Args, ", ") + ")"
	if ec != nil {
		if n.Err == ErrReturn {
			return append([]string{n.Var + ", " + n.Cleanup + ", err := " + call}, ec.errReturn()...)
		}
		return []string{n.Var + ", " + n.Cleanup + " := " + call}
	}
	var lines []string
	if n.Err == ErrReturn {
		lines = append([]string{n.Var + ", " + n.Cleanup + ", err := " + call}, ec.errReturn()...)
	} else {
		lines = []string{n.Var + ", " + n.Cleanup + " := " + call}
	}
	return append(lines,
		"if "+n.Cleanup+" != nil {",
		"defer func() {",
		"err = "+n.ErrsPkg+".Join(err, "+n.Cleanup+"())",
		"}()",
		"}")
}

func renderSelect(n *Select, ec *errCtx) []string {
	return renderSelectWithComments(n, ec, nil)
}

func renderSelectWithComments(n *Select, ec *errCtx, comment func(Node) string) []string {
	lines := []string{
		"var " + n.Var + " " + n.Iface,
		"switch " + n.KeyExpr + " {",
	}
	childComment := func(child Node) {
		if comment == nil {
			return
		}
		if c := comment(child); c != "" {
			lines = append(lines, c)
		}
	}
	for _, c := range n.Cases {
		lines = append(lines, "case "+strconv.Quote(c.Value)+":")
		for _, b := range c.Body {
			childComment(b)
			if sub, ok := b.(*Select); ok && comment != nil {
				lines = append(lines, renderSelectWithComments(sub, ec, comment)...)
				continue
			}
			lines = append(lines, renderNode(b, ec)...)
		}
		childComment(&c.Result)
		lines = append(lines, renderSelectResult(n, c, ec)...)
	}
	errorf := fmt.Sprintf("%s.Errorf(\"no %s implementation for %%q\", %s)", n.FmtPkg, n.Iface, n.KeyExpr)
	lines = append(lines, "default:")
	lines = append(lines, ec.errorExprReturn(errorf)...)
	return append(lines, "}")
}

func renderSelectResult(n *Select, c Case, ec *errCtx) []string {
	call := c.Result.Fn + "(" + strings.Join(c.Result.Args, ", ") + ")"
	if c.Result.Err == ErrReturn {
		lines := []string{c.Result.Var + ", err := " + call}
		lines = append(lines, ec.errReturn()...)
		return append(lines, n.Var+" = "+c.Result.Var)
	}
	return []string{n.Var + " = " + call}
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
		for _, v := range slices.Backward(n.Chain) {
			expr = v + "(" + expr + ")"
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

// layoutCtx indexes dependency and affinity signals across all nodes.
type layoutCtx struct {
	fanout    map[string]int
	consumers map[string][]int // variable -> consuming universe node ids
	nodeIx    map[Node]int
	next      [][]int        // node -> consumer nodes through non-hub values
	roots     []map[int]bool // node -> reachable feature roots (memoized)
}

// newLayoutCtx indexes affinity across all sections being laid out.
func newLayoutCtx(universe []Node) *layoutCtx {
	vars := map[string]bool{}
	for _, n := range universe {
		for _, d := range defines(n) {
			vars[d] = true
		}
	}
	lc := &layoutCtx{fanout: map[string]int{}, consumers: map[string][]int{}, nodeIx: map[Node]int{}}
	for i, n := range universe {
		if _, ok := lc.nodeIx[n]; !ok {
			lc.nodeIx[n] = i
		}
		seen := map[string]bool{}
		for _, u := range uses(n, vars) {
			if seen[u] {
				continue
			}
			seen[u] = true
			lc.fanout[u]++
			lc.consumers[u] = append(lc.consumers[u], i)
		}
	}
	lc.next = make([][]int, len(universe))
	for i, n := range universe {
		if lc.nodeIx[n] != i {
			continue
		}
		seen := map[int]bool{}
		for _, d := range defines(n) {
			if lc.fanout[d] > hubFanout {
				continue
			}
			for _, c := range lc.consumers[d] {
				if !seen[c] {
					seen[c] = true
					lc.next[i] = append(lc.next[i], c)
				}
			}
		}
	}
	lc.roots = make([]map[int]bool, len(universe))
	return lc
}

// featureRoots returns sinks reachable through non-hub values.
func (lc *layoutCtx) featureRoots(i int) map[int]bool {
	if lc.roots[i] != nil {
		return lc.roots[i]
	}
	out := map[int]bool{}
	lc.roots[i] = out // breaks cycles; an all-cycle path roots at itself below
	if len(lc.next[i]) == 0 {
		out[i] = true
		return out
	}
	for _, c := range lc.next[i] {
		for r := range lc.featureRoots(c) {
			out[r] = true
		}
	}
	if len(out) == 0 {
		out[i] = true
	}
	return out
}

// Affinity prefers explicit groups, batches, data flow, directive occurrence, file, then proximity.
const (
	affGroup       = 100
	affBatch       = 90
	affExclusive   = 80
	affSmallFan    = 60
	affSharedCons  = 55
	affDirective   = 50
	affNearby      = 40
	affSameFile    = 30
	affSamePkg     = 25
	affHub         = 15
	affThreshold   = 40
	layoutBlockMax = 10
	hubFanout      = 3
)

func (lc *layoutCtx) affinity(a, b Node) int {
	ab, bb := a.base(), b.base()
	if ab.Group != "" && ab.Group == bb.Group {
		return affGroup
	}
	if ab.Batch != "" && ab.Batch == bb.Batch {
		return affBatch
	}
	// Hub edges must not outweigh stronger directive or source signals.
	best := 0
	if s := lc.producerConsumer(a, b); s > best {
		best = s
	}
	if s := lc.producerConsumer(b, a); s > best {
		best = s
	}
	if best < affSharedCons {
		if s := lc.sharedConsumer(a, b); s > best {
			best = s
		}
	}
	if best < affDirective && ab.Origin.Pos.IsValid() && ab.Origin.Directive != "" &&
		ab.Origin.Directive == bb.Origin.Directive && ab.Origin.Pos == bb.Origin.Pos {
		best = affDirective
	}
	if best < affNearby && ab.Origin.Pos.IsValid() && bb.Origin.Pos.IsValid() {
		if ab.Origin.Pos.Filename == bb.Origin.Pos.Filename {
			d := ab.Origin.Pos.Line - bb.Origin.Pos.Line
			if d < 0 {
				d = -d
			}
			score := affSameFile
			if d <= 10 {
				score = affNearby
			}
			if score > best {
				best = score
			}
		} else if filepath.Dir(ab.Origin.Pos.Filename) == filepath.Dir(bb.Origin.Pos.Filename) && affSamePkg > best {
			best = affSamePkg
		}
	}
	return best
}

// producerConsumer discounts producer affinity as fan-out increases.
func (lc *layoutCtx) producerConsumer(producer, consumer Node) int {
	ci, ok := lc.nodeIx[consumer]
	if !ok {
		return 0
	}
	best := 0
	for _, d := range defines(producer) {
		for _, u := range lc.consumers[d] {
			if u != ci {
				continue
			}
			switch f := lc.fanout[d]; {
			case f == 1:
				return affExclusive
			case f <= hubFanout:
				if affSmallFan > best {
					best = affSmallFan
				}
			default:
				if affHub > best {
					best = affHub
				}
			}
		}
	}
	return best
}

// sharedConsumer scores producers that reach the same sink through non-hub values.
func (lc *layoutCtx) sharedConsumer(a, b Node) int {
	ai, aok := lc.nodeIx[a]
	bi, bok := lc.nodeIx[b]
	if !aok || !bok {
		return 0
	}
	if len(lc.next[ai]) == 0 && len(lc.next[bi]) == 0 {
		return 0
	}
	ar := lc.featureRoots(ai)
	for r := range lc.featureRoots(bi) {
		if r != ai && r != bi && ar[r] {
			return affSharedCons
		}
	}
	return 0
}

// layout preserves dependency and batch constraints while greedily grouping affine ready nodes.
// Source anchors and emission order break ties; groups define blank-line boundaries.
func (lc *layoutCtx) layout(nodes []phaseNode) [][]phaseNode {
	if len(nodes) == 0 {
		return nil
	}
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
	deps := make([][]int, len(nodes))
	for i, pn := range nodes {
		for _, u := range uses(pn.n, vars) {
			if j := owner[u]; j != i {
				deps[i] = append(deps[i], j)
			}
		}
	}
	byBatch := map[string][]int{}
	for i, pn := range nodes {
		if b := pn.n.base().Batch; b != "" {
			byBatch[b] = append(byBatch[b], i)
		}
	}
	for _, ids := range byBatch {
		sort.SliceStable(ids, func(x, y int) bool {
			return nodes[ids[x]].n.base().Seq < nodes[ids[y]].n.base().Seq
		})
		for k := 1; k < len(ids); k++ {
			deps[ids[k]] = append(deps[ids[k]], ids[k-1])
		}
	}
	emitted := make([]bool, len(nodes))
	remaining := len(nodes)
	var blocks [][]phaseNode
	var block []phaseNode
	var blockIx []int
	for remaining > 0 {
		var ready []int
		for i := range nodes {
			if emitted[i] {
				continue
			}
			ok := true
			for _, j := range deps[i] {
				if !emitted[j] {
					ok = false
					break
				}
			}
			if ok {
				ready = append(ready, i)
			}
		}
		if len(ready) == 0 {
			// A dependency cycle was already diagnosed; fall back to
			// emission order.
			for i := range nodes {
				if !emitted[i] {
					ready = []int{i}
					break
				}
			}
		}
		pick := -1
		if len(block) > 0 && len(block) < layoutBlockMax {
			bestAff := 0
			for _, i := range ready {
				aff := 0
				for _, j := range blockIx {
					if a := lc.affinity(nodes[i].n, nodes[j].n); a > aff {
						aff = a
					}
				}
				if aff < affThreshold {
					continue
				}
				if pick < 0 || aff > bestAff || (aff == bestAff && anchorLess(nodes[i], nodes[pick])) {
					pick, bestAff = i, aff
				}
			}
		}
		if pick < 0 {
			if len(block) > 0 {
				blocks = append(blocks, block)
				block, blockIx = nil, nil
			}
			for _, i := range ready {
				if pick < 0 || anchorLess(nodes[i], nodes[pick]) {
					pick = i
				}
			}
		}
		emitted[pick] = true
		remaining--
		block = append(block, nodes[pick])
		blockIx = append(blockIx, pick)
	}
	if len(block) > 0 {
		blocks = append(blocks, block)
	}
	return blocks
}
