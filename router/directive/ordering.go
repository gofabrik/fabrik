package directive

import (
	"fmt"
	"go/token"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/gofabrik/fabrik/diag"
)

// Wildcards split globals into outer, middle, and inner bands; edges must agree with band order.
const (
	bandOuter = iota
	bandMiddle
	bandInner
)

type orderedGlobal struct {
	nd    *mwNode
	label string
}

// inactiveEdge records an unresolved soft reference for inspection output.
type inactiveEdge struct {
	nd  *mwNode
	opt string
	pos token.Position
}

type orderResult struct {
	stack    []orderedGlobal
	inactive []inactiveEdge
}

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

// resolveOrder applies wildcard bands and dependency edges, breaking ties by declaration order.
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

	// Band order satisfies or rejects cross-band edges; only same-band edges enter the graph.
	graph := map[*mwNode][]mwEdge{}
	indeg := map[*mwNode]int{}
	for _, e := range edges {
		bf, bt := band(e.from), band(e.to)
		switch {
		case bf < bt:
			// Band placement already satisfies the edge.
		case bf > bt:
			ds.Error(e.pos, fmt.Sprintf("%s contradicts the wildcard band order (%s is in an earlier band)", e.opt, labels[e.decl]),
				"the edge demands an order the before=*/after=* bands already forbid")
			continue
		default:
			graph[e.from] = append(graph[e.from], e)
			indeg[e.to]++
		}
		// Record incoming relations so targeted nodes are not labeled unconstrained.
		other := e.from
		rel := "before " + labels[e.to]
		if e.decl == e.from {
			other, rel = e.to, "after "+labels[e.from]
		}
		if other != e.decl {
			incoming[other] = append(incoming[other], rel)
		}
	}

	// Exclude nodes already covered by requires-cycle diagnostics.
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

	// Sort each band topologically, breaking ties by declaration order.
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
			// Report one independent cycle, but keep the stack complete for other diagnostics.
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

// stuckCycle returns one deterministic cycle from the stuck subgraph.
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

// reasonLabel summarizes active and inactive placement constraints.
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

// validateDecls reports hard-constraint errors, inert declarations, and unused names.
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
			ds.Error(nd.pos, fmt.Sprintf("middleware %s is inert (no name, not global)", labels[nd]),
				"add global=true to run it everywhere, or name= to reference it from middleware= chains; bare declarations were global before the ordering options existed, so silence here would drop middleware on regeneration")
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

// requiresCycle returns one deterministic cycle within decls.
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

// mwLabels returns checkout-independent display names unique within decls.
func mwLabels(decls []*mwNode) map[*mwNode]string {
	labels := make(map[*mwNode]string, len(decls))
	count := map[string]int{}
	for _, nd := range decls {
		labels[nd] = displayName(nd)
		count[labels[nd]]++
	}
	again := map[string]int{}
	for _, nd := range decls {
		if count[labels[nd]] != 1 {
			labels[nd] = fmt.Sprintf("%s (%s:%d)", labels[nd], filepath.Base(nd.pos.Filename), nd.pos.Line)
		}
		again[labels[nd]]++
	}
	// Add the import path only when file and line still collide.
	for _, nd := range decls {
		if again[labels[nd]] != 1 && nd.pkg != nil {
			labels[nd] = fmt.Sprintf("%s (%s/%s:%d)", displayName(nd), nd.pkg.Path(), filepath.Base(nd.pos.Filename), nd.pos.Line)
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
