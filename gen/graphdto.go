package gen

import (
	"fmt"
	"go/types"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// graphSchemaVersion identifies a schema whose existing fields never change meaning.
const graphSchemaVersion = 1

// Graph is the structural export of a generated app: its flows, the
// nodes each flow emits, and the dependency edges between them.
type Graph struct {
	Version int         `json:"version"`
	Module  string      `json:"module"`
	Flows   []GraphFlow `json:"flows,omitempty"`
	Nodes   []GraphNode `json:"nodes,omitempty"`
	Edges   []GraphEdge `json:"edges,omitempty"`
}

// GraphFlow is one build function: a command scope, or the unscoped
// default flow.
type GraphFlow struct {
	ID       string         `json:"id"`
	Fn       string         `json:"fn,omitempty"`
	Bindings []GraphBinding `json:"bindings,omitempty"`
	Roots    []GraphRoot    `json:"roots,omitempty"`
}

// GraphType carries canonical identity and the display form used in generated source.
type GraphType struct {
	Display   string `json:"display"`
	Canonical string `json:"canonical"`
	Name      string `json:"name,omitempty"` // Provider name, empty for the unnamed binding.
}

// GraphRoot references either an expression or a binding that owns its dependency edges.
type GraphRoot struct {
	ID      string    `json:"id"`
	Type    GraphType `json:"type"`
	Name    string    `json:"name,omitempty"` // Provider name selected for the root.
	Expr    string    `json:"expr,omitempty"`
	Binding string    `json:"binding,omitempty"`
}

// GraphBinding is an inline constructor expression with every bound type and path.
type GraphBinding struct {
	ID    string      `json:"id"`
	Expr  string      `json:"expr"`
	Types []GraphType `json:"types,omitempty"`
	Paths []string    `json:"paths,omitempty"`
}

// GraphNode is one emitted statement.
type GraphNode struct {
	ID            string   `json:"id"`
	Flow          string   `json:"flow"`
	Kind          string   `json:"kind"`
	Defines       []string `json:"defines,omitempty"`
	Fn            string   `json:"fn,omitempty"`
	Type          string   `json:"type,omitempty"`
	TypeCanonical string   `json:"typeCanonical,omitempty"` // Set when the IR carries a package-path type.
	Cleanup       string   `json:"cleanup,omitempty"`
	BindingName   string   `json:"bindingName,omitempty"` // Provider name bound by this call.
	Method        string   `json:"method,omitempty"`
	Pattern       string   `json:"pattern,omitempty"`
	Directive     string   `json:"directive"`
	Pos           string   `json:"pos,omitempty"`
	Label         string   `json:"label,omitempty"`
	Parent        string   `json:"parent,omitempty"`
	Case          string   `json:"case,omitempty"`
	Role          string   `json:"role,omitempty"` // "result" identifies a Select constructor.
}

// GraphEdge points from a consumer to what it depends on.
type GraphEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Graph exports the structural graph and must run after [Gen.Render] finalizes aliases; it never registers imports.
func (g *Gen) Graph() *Graph {
	gr := &Graph{Version: graphSchemaVersion, Module: g.module}
	seen := map[GraphEdge]bool{}
	gr.addFlow(g, "default", "run", emissionOrdered(g.nodes), nil, seen)
	used := map[string]bool{"default": true}
	for _, s := range g.scopes {
		// A command named "default" uses its build function name to avoid the reserved flow id.
		id := flowID(s.fn)
		if used[id] {
			id = s.fn
		}
		for n := 2; used[id]; n++ {
			id = fmt.Sprintf("%s@%d", s.fn, n)
		}
		used[id] = true
		gr.addFlow(g, id, s.fn, emissionOrdered(s.nodes), s, seen)
	}
	sort.Slice(gr.Edges, func(i, j int) bool {
		if gr.Edges[i].From != gr.Edges[j].From {
			return gr.Edges[i].From < gr.Edges[j].From
		}
		return gr.Edges[i].To < gr.Edges[j].To
	})
	return gr
}

type flowNode struct {
	n      Node
	id     string
	parent string
	branch string
	result bool
}

func (gr *Graph) addFlow(g *Gen, id, fn string, nodes []Node, s *Scope, seen map[GraphEdge]bool) {
	// Nodeless scopes have no build function.
	if s != nil && len(nodes) == 0 {
		fn = ""
	}
	flow := GraphFlow{ID: id, Fn: fn}

	var list []flowNode
	owner := map[string]string{}
	kinds := map[string]int{}
	var walk func(ns []Node, parent, branch string, result bool)
	walk = func(ns []Node, parent, branch string, result bool) {
		for _, n := range ns {
			kind := nodeKind(n)
			nodeID := fmt.Sprintf("%s/%s@%d", id, kind, kinds[kind])
			if defs := graphDefines(n); len(defs) > 0 {
				nodeID = id + "/" + defs[0]
			}
			kinds[kind]++
			for _, d := range defines(n) {
				if _, taken := owner[d]; !taken {
					owner[d] = nodeID
				}
			}
			list = append(list, flowNode{n: n, id: nodeID, parent: parent, branch: branch, result: result})
			if sel, ok := n.(*Select); ok {
				for _, c := range sel.Cases {
					walk(c.Body, nodeID, c.Value, false)
					res := c.Result
					walk([]Node{&res}, nodeID, c.Value, true)
				}
			}
		}
	}
	walk(nodes, "", "", false)

	bindings, byExpr := flowBindings(g, id, s, owner)
	flow.Bindings = bindings

	edge := func(from, to string) {
		if from == "" || to == "" || from == to {
			return
		}
		e := GraphEdge{From: from, To: to}
		if seen[e] {
			return
		}
		seen[e] = true
		gr.Edges = append(gr.Edges, e)
	}
	// Exact binding expressions target the binding; other expressions contribute free identifiers.
	depend := func(from, expr string, own map[string]bool) {
		if bind, ok := byExpr[expr]; ok {
			edge(from, bind)
			return
		}
		freeIdents(expr, func(name string) {
			if !own[name] {
				edge(from, owner[name])
			}
		})
	}

	for _, b := range flow.Bindings {
		// A binding expression identifies itself and cannot create a reference.
		freeIdents(b.Expr, func(name string) { edge(b.ID, owner[name]) })
	}
	if s != nil {
		for i, r := range s.roots {
			t := types.Unalias(r.Type)
			root := GraphRoot{
				ID:   fmt.Sprintf("%s/root@%d", id, i),
				Type: GraphType{Display: g.TypeDisplay(t), Canonical: types.TypeString(t, nil)},
				Name: r.Name,
			}
			expr := ""
			if i < len(s.rootExprs) {
				expr = s.rootExprs[i]
			}
			if bind, ok := byExpr[expr]; ok {
				root.Binding = bind
			} else {
				root.Expr = expr
				depend(root.ID, expr, nil)
			}
			flow.Roots = append(flow.Roots, root)
		}
	}

	for _, fnode := range list {
		gr.Nodes = append(gr.Nodes, graphNode(g, fnode, id))
		own := map[string]bool{}
		for _, d := range defines(fnode.n) {
			own[d] = true
		}
		for _, expr := range exprFields(fnode.n) {
			depend(fnode.id, expr, own)
		}
		for _, u := range fnode.n.base().Uses {
			if !own[u] {
				edge(fnode.id, owner[u])
			}
		}
	}

	gr.Flows = append(gr.Flows, flow)
}

// flowBindings preserves first-bind order and indexes bindings by expression.
func flowBindings(g *Gen, flow string, s *Scope, owner map[string]string) ([]GraphBinding, map[string]string) {
	typesByExpr := map[string][]GraphType{}
	pathsByExpr := map[string][]string{}
	addType := func(expr string, t types.Type, canonical, name string) {
		gt := GraphType{Display: g.TypeDisplay(t), Canonical: canonical, Name: name}
		if !slices.Contains(typesByExpr[expr], gt) {
			typesByExpr[expr] = append(typesByExpr[expr], gt)
		}
	}
	var order []string
	if s != nil {
		order = s.bindOrder
		for key, byName := range s.binds {
			for name, expr := range byName {
				addType(expr, s.bindTypes[key], key, name)
			}
		}
		for path, expr := range s.pathExprs {
			if !slices.Contains(pathsByExpr[expr], path) {
				pathsByExpr[expr] = append(pathsByExpr[expr], path)
			}
		}
	} else {
		order = g.bindOrder
		g.binds.Iterate(func(t types.Type, v any) {
			key := types.TypeString(t, nil)
			for name, expr := range v.(map[string]string) {
				addType(expr, t, key, name)
			}
		})
		for path, expr := range g.pathExprs {
			if !slices.Contains(pathsByExpr[expr], path) {
				pathsByExpr[expr] = append(pathsByExpr[expr], path)
			}
		}
	}
	for _, ts := range typesByExpr {
		sort.Slice(ts, func(i, j int) bool {
			if ts[i].Canonical != ts[j].Canonical {
				return ts[i].Canonical < ts[j].Canonical
			}
			return ts[i].Name < ts[j].Name
		})
	}
	for _, ps := range pathsByExpr {
		sort.Strings(ps)
	}

	var out []GraphBinding
	byExpr := map[string]string{}
	for _, expr := range order {
		if expr == "" || owner[expr] != "" {
			// A bare generated variable is the node that defines it.
			continue
		}
		if _, dup := byExpr[expr]; dup {
			continue
		}
		ts, ps := typesByExpr[expr], pathsByExpr[expr]
		if len(ts) == 0 && len(ps) == 0 {
			continue
		}
		id := fmt.Sprintf("%s/bind@%d", flow, len(out))
		byExpr[expr] = id
		out = append(out, GraphBinding{ID: id, Expr: expr, Types: ts, Paths: ps})
	}
	return out, byExpr
}

// emissionOrdered mirrors renderer phase and cluster order.
func emissionOrdered(nodes []Node) []Node {
	out := make([]Node, 0, len(nodes))
	for _, pl := range phaseLabels {
		var pns []phaseNode
		for i, n := range nodes {
			if n.base().Phase == pl.phase {
				pns = append(pns, phaseNode{n: n, emit: i})
			}
		}
		for _, cluster := range layoutPhase(pns) {
			for _, pn := range cluster {
				out = append(out, pn.n)
			}
		}
	}
	return out
}

func graphNode(g *Gen, fnode flowNode, flow string) GraphNode {
	o := fnode.n.base().Origin
	out := GraphNode{
		ID:        fnode.id,
		Flow:      flow,
		Kind:      nodeKind(fnode.n),
		Defines:   graphDefines(fnode.n),
		Directive: o.Directive,
		Pos:       g.graphPos(o),
		Label:     fnode.n.base().Label,
		Parent:    fnode.parent,
		Case:      fnode.branch,
	}
	if fnode.result {
		out.Role = "result"
	}
	switch n := fnode.n.(type) {
	case *Call:
		out.Fn, out.Cleanup = n.Fn, n.Cleanup
		out.BindingName = n.BindingName
		if n.Type != nil {
			out.Type = g.TypeDisplay(n.Type)
			out.TypeCanonical = types.TypeString(n.Type, nil)
		}
	case *ConfigLoad:
		out.Type = "*" + n.Type
	case *StructLit:
		out.Type = "*" + n.Type
	case *Select:
		out.Type = n.Iface
	case *Route:
		out.Method, out.Pattern = n.Method, n.Pattern
	}
	return out
}

func graphDefines(n Node) []string {
	defs := defines(n)
	if c, ok := n.(*Call); ok && c.Cleanup != "" && len(defs) > 1 {
		defs = defs[:1]
	}
	return defs
}

func nodeKind(n Node) string {
	switch n.(type) {
	case *Raw:
		return "raw"
	case *Assign:
		return "assign"
	case *Call:
		return "call"
	case *ConfigLoad:
		return "configLoad"
	case *StructLit:
		return "structLit"
	case *Select:
		return "select"
	case *Route:
		return "route"
	}
	return "node"
}

func flowID(fn string) string {
	if rest := strings.TrimPrefix(fn, "build"); rest != fn && rest != "" {
		return LowerFirst(rest)
	}
	return fn
}

// graphPos emits module-relative positions so artifacts never contain absolute paths.
func (g *Gen) graphPos(o Origin) string {
	if !o.Pos.IsValid() {
		return ""
	}
	file := o.Pos.Filename
	if g.srcRoot != "" {
		if rel, err := filepath.Rel(g.srcRoot, file); err == nil {
			file = rel
		}
	}
	return fmt.Sprintf("%s:%d", filepath.ToSlash(file), o.Pos.Line)
}

// DOT renders the graph as a Graphviz digraph, one cluster per flow.
func (gr *Graph) DOT() string {
	var b strings.Builder
	b.WriteString("digraph fabrik {\n")
	b.WriteString("  rankdir=TB;\n")
	b.WriteString("  node [shape=box, fontname=\"monospace\", fontsize=10];\n")
	rendered := gr.renderedIDs()
	for fi, f := range gr.Flows {
		if !gr.flowRendered(f, rendered) {
			continue
		}
		fmt.Fprintf(&b, "\n  subgraph cluster_f%d {\n", fi)
		fmt.Fprintf(&b, "    label=%q;\n", flowTitle(f))
		for _, bind := range f.Bindings {
			fmt.Fprintf(&b, "    %q [label=%q, shape=ellipse];\n", bind.ID, bindingLabel(bind, "\n"))
		}
		for _, r := range f.Roots {
			fmt.Fprintf(&b, "    %q [label=%q, shape=diamond];\n", r.ID, rootLabel(r))
		}
		for _, n := range gr.Nodes {
			if n.Flow != f.ID || !gr.architectural(n) {
				continue
			}
			fmt.Fprintf(&b, "    %q [label=%q];\n", n.ID, strings.Join(nodeLabel(n, "  "), "\n"))
		}
		b.WriteString("  }\n")
	}
	b.WriteString("\n")
	for _, e := range gr.Edges {
		if !rendered[e.From] || !rendered[e.To] {
			continue
		}
		fmt.Fprintf(&b, "  %q -> %q;\n", e.From, e.To)
	}
	for _, f := range gr.Flows {
		for _, r := range f.Roots {
			if r.Binding != "" {
				fmt.Fprintf(&b, "  %q -> %q [style=dotted];\n", r.ID, r.Binding)
			}
		}
	}
	b.WriteString("}\n")
	return b.String()
}

// Mermaid renders one subgraph per flow with sequential IDs that cannot collide after sanitization.
func (gr *Graph) Mermaid() string {
	ids := map[string]string{}
	mid := func(id string) string {
		if m, ok := ids[id]; ok {
			return m
		}
		m := fmt.Sprintf("n%d", len(ids))
		ids[id] = m
		return m
	}
	var b strings.Builder
	b.WriteString("flowchart TB\n")
	rendered := gr.renderedIDs()
	for fi, f := range gr.Flows {
		if !gr.flowRendered(f, rendered) {
			continue
		}
		fmt.Fprintf(&b, "  subgraph f%d[%q]\n", fi, flowTitle(f))
		for _, bind := range f.Bindings {
			fmt.Fprintf(&b, "    %s(%q)\n", mid(bind.ID), mermaidText(bindingLabel(bind, "<br/>")))
		}
		for _, r := range f.Roots {
			fmt.Fprintf(&b, "    %s{{%q}}\n", mid(r.ID), mermaidText(rootLabel(r)))
		}
		for _, n := range gr.Nodes {
			if n.Flow != f.ID || !gr.architectural(n) {
				continue
			}
			fmt.Fprintf(&b, "    %s[%q]\n", mid(n.ID), mermaidText(strings.Join(nodeLabel(n, " "), "<br/>")))
		}
		b.WriteString("  end\n")
	}
	for _, e := range gr.Edges {
		if !rendered[e.From] || !rendered[e.To] {
			continue
		}
		fmt.Fprintf(&b, "  %s --> %s\n", mid(e.From), mid(e.To))
	}
	for _, f := range gr.Flows {
		for _, r := range f.Roots {
			if r.Binding != "" {
				fmt.Fprintf(&b, "  %s -.-> %s\n", mid(r.ID), mid(r.Binding))
			}
		}
	}
	return b.String()
}

// architectural omits registration effects from diagrams while preserving them in JSON.
func (gr *Graph) architectural(n GraphNode) bool {
	return len(n.Defines) > 0 || n.Role == "result"
}

func flowTitle(f GraphFlow) string {
	if f.Fn != "" {
		return f.Fn
	}
	return f.ID
}

// flowRendered omits flows with no rendered bindings, roots, or nodes.
func (gr *Graph) flowRendered(f GraphFlow, rendered map[string]bool) bool {
	if len(f.Bindings) > 0 || len(f.Roots) > 0 {
		return true
	}
	for _, n := range gr.Nodes {
		if n.Flow == f.ID && rendered[n.ID] {
			return true
		}
	}
	return false
}

// renderedIDs prevents edges from referencing nodes omitted from diagrams.
func (gr *Graph) renderedIDs() map[string]bool {
	ids := map[string]bool{}
	for _, f := range gr.Flows {
		for _, b := range f.Bindings {
			ids[b.ID] = true
		}
		for _, r := range f.Roots {
			ids[r.ID] = true
		}
	}
	for _, n := range gr.Nodes {
		if gr.architectural(n) {
			ids[n.ID] = true
		}
	}
	return ids
}

func nodeLabel(n GraphNode, gap string) []string {
	name := n.ID
	switch {
	case len(n.Defines) > 0:
		name = n.Defines[0]
	case n.Kind == "route":
		name = strings.TrimSpace(n.Method + " " + n.Pattern)
	case n.Fn != "":
		name = n.Fn
	}
	lines := []string{name}
	if typ := strings.TrimSpace(n.Type + cleanupMark(n.Cleanup)); typ != "" {
		lines = append(lines, typ)
	}
	if n.Directive != "" {
		line := n.Directive
		if n.Pos != "" {
			line += gap + n.Pos
		}
		lines = append(lines, line)
	}
	return lines
}

func cleanupMark(cleanup string) string {
	if cleanup == "" {
		return ""
	}
	return " (cleanup)"
}

func bindingLabel(b GraphBinding, sep string) string {
	lines := []string{b.Expr}
	if len(b.Types) > 0 {
		lines = append(lines, b.Types[0].Display)
	} else if len(b.Paths) > 0 {
		lines = append(lines, b.Paths[0])
	}
	return strings.Join(lines, sep)
}

func rootLabel(r GraphRoot) string {
	return "root: " + r.Type.Display
}

func mermaidText(s string) string {
	return strings.ReplaceAll(s, `"`, "#quot;")
}
