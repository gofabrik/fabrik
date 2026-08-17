package directive

import (
	"fmt"
	"go/token"
	"slices"
	"sort"
	"strings"

	"github.com/gofabrik/fabrik/diag"
)

// Wildcard bands for global middleware: before=* is the outer band,
// after=* the inner; everything else sits between. Edges never cross
// bands: a cross-band edge is either satisfied by band order (kept as
// a recorded reason) or a generation error.
const (
	bandOuter = iota
	bandMiddle
	bandInner
)

// orderedGlobal is one resolved global stack entry with its
// placement-reason label.
type orderedGlobal struct {
	nd    *mwNode
	label string
}

// inactiveEdge is a soft ordering reference whose target no
// declaration provides. Generation stays silent; the inspection
// surfaces carry it.
type inactiveEdge struct {
	nd  *mwNode
	opt string
	pos token.Position
}

type orderResult struct {
	stack    []orderedGlobal
	inactive []inactiveEdge
}

// mwEdge is one from-before-to ordering relation, tagged with the
// declaring node and the option text that produced it.
type mwEdge struct {
	from, to *mwNode
	decl     *mwNode
	opt      string
	pos      token.Position
	hard     bool
}

func band(nd *mwNode) int {
	switch {
	case nd.beforeAll:
		return bandOuter
	case nd.afterAll:
		return bandInner
	}
	return bandMiddle
}

// resolveOrder orders the global middleware stack from declared
// constraints: requires= (hard, implies after), soft after=/before=,
// and the before=*/after=* bands, falling back to loader order
// (file, line). Unknown hard targets, global-requires-non-global, and
// pure requires= cycles are validateDecls' findings; this resolver
// skips those edges and stays quiet about those cycles so each is
// reported exactly once.
func resolveOrder(globals, decls []*mwNode, byName map[string]*mwNode, labels map[*mwNode]string) (orderResult, diag.Diagnostics) {
	var res orderResult
	var ds diag.Diagnostics
	incoming := map[*mwNode][]string{}

	var edges []mwEdge
	inactive := func(g *mwNode, opt string, pos token.Position) {
		res.inactive = append(res.inactive, inactiveEdge{nd: g, opt: opt, pos: pos})
	}
	for _, g := range globals {
		for _, r := range g.requires {
			target := byName[r.name]
			if target == nil || !target.global {
				continue
			}
			edges = append(edges, mwEdge{from: target, to: g, decl: g, opt: "requires=" + r.name, pos: r.pos, hard: true})
		}
		for _, r := range g.after {
			target := byName[r.name]
			if target == nil {
				inactive(g, "after="+r.name, r.pos)
				continue
			}
			if !target.global {
				ds.Error(r.pos, fmt.Sprintf("after=%s: middleware %q is not global", r.name, r.name),
					"ordering applies to the global stack; route middleware order is the middleware= list")
				continue
			}
			edges = append(edges, mwEdge{from: target, to: g, decl: g, opt: "after=" + r.name, pos: r.pos})
		}
		for _, r := range g.before {
			target := byName[r.name]
			if target == nil {
				inactive(g, "before="+r.name, r.pos)
				continue
			}
			if !target.global {
				ds.Error(r.pos, fmt.Sprintf("before=%s: middleware %q is not global", r.name, r.name),
					"ordering applies to the global stack; route middleware order is the middleware= list")
				continue
			}
			edges = append(edges, mwEdge{from: g, to: target, decl: g, opt: "before=" + r.name, pos: r.pos})
		}
	}

	// Cross-band edges: band order either already satisfies the edge
	// (kept as a reason) or contradicts it (an error, hard and soft
	// alike). Only same-band edges enter the toposort graph.
	graph := map[*mwNode][]mwEdge{}
	indeg := map[*mwNode]int{}
	for _, e := range edges {
		bf, bt := band(e.from), band(e.to)
		switch {
		case bf < bt:
			// Satisfied by band placement; the labels keep the relation.
		case bf > bt:
			ds.Error(e.pos, fmt.Sprintf("%s contradicts the wildcard band order (%s is in an earlier band)", e.opt, labels[e.decl]),
				"the edge demands an order the before=*/after=* bands already forbid")
			continue
		default:
			graph[e.from] = append(graph[e.from], e)
			indeg[e.to]++
		}
		// The non-declaring endpoint records the relation too, so a
		// targeted node never reads as unconstrained.
		other := e.from
		rel := "before " + labels[e.to]
		if e.decl == e.from {
			other, rel = e.to, "after "+labels[e.from]
		}
		if other != e.decl {
			incoming[other] = append(incoming[other], rel)
		}
	}

	// Nodes on any pure requires= cycle are validateDecls' findings;
	// computed over every declaration (the same graph validateDecls
	// walks) so a cycle reported below never overlaps an
	// already-reported node set, whatever placements or bands its
	// members have.
	condemned := map[*mwNode]bool{}
	remaining := append([]*mwNode(nil), decls...)
	for {
		cyc := requiresCycle(remaining, byName)
		if cyc == nil {
			break
		}
		inCyc := map[*mwNode]bool{}
		for _, nd := range cyc {
			inCyc[nd] = true
			condemned[nd] = true
		}
		var rest []*mwNode
		for _, nd := range remaining {
			if !inCyc[nd] {
				rest = append(rest, nd)
			}
		}
		remaining = rest
	}

	// Kahn per band, ready queue in loader order (file, line).
	byBand := map[int][]*mwNode{}
	for _, g := range globals {
		byBand[band(g)] = append(byBand[band(g)], g)
	}
	cycleReported := false
	for _, b := range []int{bandOuter, bandMiddle, bandInner} {
		members := append([]*mwNode(nil), byBand[b]...)
		sort.SliceStable(members, func(i, j int) bool { return declLess(members[i], members[j]) })
		deg := map[*mwNode]int{}
		for _, nd := range members {
			deg[nd] = indeg[nd]
		}
		emitted := 0
		for {
			next := (*mwNode)(nil)
			for _, nd := range members {
				if deg[nd] == 0 {
					next = nd
					break
				}
			}
			if next == nil {
				break
			}
			deg[next] = -1
			emitted++
			res.stack = append(res.stack, orderedGlobal{nd: next, label: reasonLabel(next, incoming[next], res.inactive)})
			for _, e := range graph[next] {
				if band(e.to) == b {
					deg[e.to]--
				}
			}
		}
		if emitted < len(members) {
			// Report one non-requires cycle (pure requires= cycles are
			// validateDecls' findings), then emit the stuck members in
			// declaration order anyway: the stack stays complete and
			// unrelated diagnostics still surface; the cycle error
			// aborts generation.
			stuck := map[*mwNode]bool{}
			var stuckList []*mwNode
			for _, nd := range members {
				if deg[nd] == 0 {
					continue
				}
				if deg[nd] > 0 {
					stuck[nd] = true
				}
				stuckList = append(stuckList, nd)
			}
			for nd := range stuck {
				if condemned[nd] {
					delete(stuck, nd)
				}
			}
			if !cycleReported {
				if cycle := stuckCycle(stuck, graph); cycle != nil {
					trace := make([]string, 0, len(cycle)+1)
					for _, e := range cycle {
						trace = append(trace, labels[e.from])
					}
					trace = append(trace, labels[cycle[0].from])
					ds.Error(cycle[0].pos, "middleware ordering cycle: "+strings.Join(trace, " -> "),
						"break the cycle by removing a requires= or after=/before= edge")
					cycleReported = true
				}
			}
			for _, nd := range stuckList {
				if deg[nd] > 0 {
					res.stack = append(res.stack, orderedGlobal{nd: nd, label: reasonLabel(nd, incoming[nd], res.inactive)})
				}
			}
		}
	}
	return res, ds
}

// stuckCycle finds one cycle inside the stuck subgraph by DFS with
// backtracking, deterministically (declaration-order roots and edges).
func stuckCycle(stuck map[*mwNode]bool, graph map[*mwNode][]mwEdge) []mwEdge {
	nodes := make([]*mwNode, 0, len(stuck))
	for nd := range stuck {
		nodes = append(nodes, nd)
	}
	sort.SliceStable(nodes, func(i, j int) bool { return declLess(nodes[i], nodes[j]) })
	const (
		white = iota
		gray
		black
	)
	color := map[*mwNode]int{}
	var path []mwEdge
	var cycle []mwEdge
	var dfs func(nd *mwNode) bool
	dfs = func(nd *mwNode) bool {
		color[nd] = gray
		for _, e := range graph[nd] {
			if !stuck[e.to] {
				continue
			}
			switch color[e.to] {
			case gray:
				for i, pe := range path {
					if pe.from == e.to {
						cycle = append(append([]mwEdge(nil), path[i:]...), e)
						return true
					}
				}
				cycle = []mwEdge{e}
				return true
			case white:
				path = append(path, e)
				if dfs(e.to) {
					return true
				}
				path = path[:len(path)-1]
			}
		}
		color[nd] = black
		return false
	}
	for _, nd := range nodes {
		if color[nd] == white && dfs(nd) {
			return cycle
		}
	}
	return nil
}

// reasonLabel renders a node's own declared constraints plus the
// relations other declarations target it with, sorted; inactive soft
// edges stay visible. Unconstrained appears only when the node
// declares nothing and nothing targets it.
func reasonLabel(nd *mwNode, incoming []string, inactive []inactiveEdge) string {
	parts := append([]string(nil), incoming...)
	if nd.beforeAll {
		parts = append(parts, "before=*")
	}
	if nd.afterAll {
		parts = append(parts, "after=*")
	}
	dead := map[string]bool{}
	for _, ie := range inactive {
		if ie.nd == nd {
			dead[ie.opt] = true
		}
	}
	for _, r := range nd.requires {
		parts = append(parts, "requires="+r.name)
	}
	for _, r := range nd.after {
		opt := "after=" + r.name
		if dead[opt] {
			opt += " (inactive)"
		}
		parts = append(parts, opt)
	}
	for _, r := range nd.before {
		opt := "before=" + r.name
		if dead[opt] {
			opt += " (inactive)"
		}
		parts = append(parts, opt)
	}
	if len(parts) == 0 {
		return "unconstrained (file order)"
	}
	sort.Strings(parts)
	return strings.Join(slices.Compact(parts), "; ")
}

// validateDecls checks declaration-level invariants for every
// middleware declaration, referenced or not: requires targets must
// exist, a global may not require a non-global, and the requires
// graph must be acyclic. Warnings: a named non-global never
// referenced, and a bare declaration (no name, not global) that
// nothing can ever run.
func validateDecls(decls []*mwNode, byName map[string]*mwNode, labels map[*mwNode]string) diag.Diagnostics {
	var ds diag.Diagnostics
	for _, nd := range decls {
		for _, r := range nd.requires {
			target := byName[r.name]
			if target == nil {
				ds.Error(r.pos, fmt.Sprintf("requires=%s: unknown middleware %q", r.name, r.name),
					"declare it: //fabrik:http:middleware name="+r.name)
				continue
			}
			if nd.global && !target.global {
				ds.Error(r.pos, fmt.Sprintf("global middleware %s requires=%s which is not global", labels[nd], r.name),
					"a global cannot depend on route-scoped middleware")
			}
		}
		if nd.name == "" && !nd.global {
			ds.Warn(nd.pos, fmt.Sprintf("middleware %s is inert (no name, not global)", labels[nd]),
				"add global=true to run it everywhere, or name= to reference it from middleware= chains")
		}
		if nd.name != "" && !nd.global && !nd.used {
			ds.Warn(nd.pos, fmt.Sprintf("middleware %q is never referenced", nd.name),
				"list it in a middleware= chain, or add global=true to run it everywhere")
		}
	}

	remaining := append([]*mwNode(nil), decls...)
	for {
		cycle := requiresCycle(remaining, byName)
		if cycle == nil {
			break
		}
		trace := make([]string, 0, len(cycle)+1)
		for _, nd := range cycle {
			trace = append(trace, labels[nd])
		}
		trace = append(trace, labels[cycle[0]])
		pos := cycle[0].pos
		next := cycle[(0+1)%len(cycle)]
		for _, r := range cycle[0].requires {
			if byName[r.name] == next {
				pos = r.pos
				break
			}
		}
		ds.Error(pos, "requires= cycle: "+strings.Join(trace, " -> "),
			"break the cycle by removing a requires= edge")
		inCycle := map[*mwNode]bool{}
		for _, nd := range cycle {
			inCycle[nd] = true
		}
		var rest []*mwNode
		for _, nd := range remaining {
			if !inCycle[nd] {
				rest = append(rest, nd)
			}
		}
		remaining = rest
	}
	return ds
}

// requiresCycle finds one cycle in the requires= graph over the given
// declarations only, deterministically (declaration order roots and
// edges). Targets outside the set are ignored so iterative cycle
// removal terminates.
func requiresCycle(decls []*mwNode, byName map[string]*mwNode) []*mwNode {
	member := make(map[*mwNode]bool, len(decls))
	for _, nd := range decls {
		member[nd] = true
	}
	sorted := append([]*mwNode(nil), decls...)
	sort.SliceStable(sorted, func(i, j int) bool { return declLess(sorted[i], sorted[j]) })
	const (
		white = iota
		gray
		black
	)
	color := map[*mwNode]int{}
	var cycle []*mwNode
	var stack []*mwNode
	var dfs func(nd *mwNode) bool
	dfs = func(nd *mwNode) bool {
		color[nd] = gray
		stack = append(stack, nd)
		for _, r := range nd.requires {
			target := byName[r.name]
			if target == nil || !member[target] {
				continue
			}
			switch color[target] {
			case gray:
				for i, s := range stack {
					if s == target {
						cycle = append([]*mwNode(nil), stack[i:]...)
						return true
					}
				}
			case white:
				if dfs(target) {
					return true
				}
			}
		}
		color[nd] = black
		stack = stack[:len(stack)-1]
		return false
	}
	for _, nd := range sorted {
		if color[nd] == white && dfs(nd) {
			return cycle
		}
	}
	return nil
}

// mwLabels builds display labels: name= when present, else pkg.Fn,
// disambiguated with file:line on collision.
func mwLabels(decls []*mwNode) map[*mwNode]string {
	labels := make(map[*mwNode]string, len(decls))
	count := map[string]int{}
	for _, nd := range decls {
		labels[nd] = displayName(nd)
		count[labels[nd]]++
	}
	for _, nd := range decls {
		if count[labels[nd]] > 1 {
			labels[nd] = fmt.Sprintf("%s (%s:%d)", labels[nd], nd.pos.Filename, nd.pos.Line)
		}
	}
	return labels
}

func displayName(nd *mwNode) string {
	if nd.name != "" {
		return nd.name
	}
	if nd.pkg != nil {
		return nd.pkg.Name() + "." + nd.fn
	}
	return nd.fn
}

func declLess(a, b *mwNode) bool {
	if a.pos.Filename != b.pos.Filename {
		return a.pos.Filename < b.pos.Filename
	}
	return a.pos.Line < b.pos.Line
}
