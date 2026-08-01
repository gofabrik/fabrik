package gen

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"

	"github.com/gofabrik/fabrik/diag"
)

// ValidateGraph checks definition, dependency, Raw metadata, and Select phase invariants.
// Rendering assumes these checks pass.
func (g *Gen) ValidateGraph() diag.Diagnostics {
	var ds diag.Diagnostics
	// Positionless nodes inherit their materialization ancestor or flow position.
	ancestor := map[Node]token.Position{}
	var mark func(n Node, pos token.Position)
	mark = func(n Node, pos token.Position) {
		ancestor[n] = pos
		if sel, ok := n.(*Select); ok {
			for ci := range sel.Cases {
				for _, child := range sel.Cases[ci].Body {
					mark(child, pos)
				}
				mark(&sel.Cases[ci].Result, pos)
			}
		}
	}
	for _, sn := range g.frag.nodes {
		if sn.pos.IsValid() {
			mark(sn.n, sn.pos)
		}
	}
	validate := func(nodes []Node, declPos token.Position) {
		g.validateFlowWith(&ds, nodes, func(n Node) token.Position {
			if p, ok := ancestor[n]; ok {
				return p
			}
			return declPos
		})
	}
	validate(g.flowNodes("default"), token.Position{})
	for _, s := range g.scopes {
		validate(g.flowNodes(s.fn), s.pos)
	}
	return ds
}

func (g *Gen) validateFlowWith(ds *diag.Diagnostics, nodes []Node, fallbackFor func(Node) token.Position) {
	nodePos := func(n Node) token.Position {
		if p := n.base().Origin.Pos; p.IsValid() {
			return p
		}
		return fallbackFor(n)
	}

	defined := map[string]token.Position{}
	var walkDefines func(ns []Node)
	walkDefines = func(ns []Node) {
		for _, n := range ns {
			for _, d := range defines(n) {
				if first, dup := defined[d]; dup {
					help := ""
					if first.IsValid() {
						help = fmt.Sprintf("first defined at %s", first)
					}
					ds.Error(nodePos(n), fmt.Sprintf("generated variable %q defined twice in one flow", d), help)
					continue
				}
				defined[d] = nodePos(n)
			}

		}
	}
	walkDefines(nodes)

	known := func(name string) bool {
		if _, ok := defined[name]; ok {
			return true
		}
		return name == g.ctxVar || name == "ctx"
	}

	var checkNode func(n Node, knownLocal func(string) bool, parentPhase Phase, inSelect bool)
	checkNode = func(n Node, knownLocal func(string) bool, parentPhase Phase, inSelect bool) {
		for _, u := range n.base().Uses {
			if !knownLocal(u) {
				ds.Error(nodePos(n), fmt.Sprintf("Uses entry %q names no generated variable in this flow", u),
					"declared dependencies must match a definition")
			}
		}
		if inSelect && n.base().Phase != parentPhase {
			ds.Error(nodePos(n), fmt.Sprintf("node phase %d differs from its parent select's phase %d", n.base().Phase, parentPhase),
				"children inherit the select's phase")
		}
		if r, ok := n.(*Raw); ok {
			validateRawMetadata(ds, r, nodePos(n), knownLocal)
		}
		if sel, ok := n.(*Select); ok {
			for ci := range sel.Cases {
				c := &sel.Cases[ci]
				caseDefined := map[string]bool{}
				caseKnown := func(name string) bool { return caseDefined[name] || knownLocal(name) }
				for _, child := range c.Body {
					for _, d := range defines(child) {
						if caseDefined[d] {
							ds.Error(nodePos(child), fmt.Sprintf("generated variable %q defined twice in one case", d), "")
						}
						caseDefined[d] = true
					}
					checkNode(child, caseKnown, sel.Phase, true)
				}
				res := &c.Result
				for _, u := range res.Uses {
					if !caseKnown(u) {
						ds.Error(nodePos(res), fmt.Sprintf("Uses entry %q names no generated variable in this flow", u),
							"declared dependencies must match a definition")
					}
				}
				if res.Phase != sel.Phase {
					ds.Error(nodePos(res), fmt.Sprintf("case result phase %d differs from its parent select's phase %d", res.Phase, sel.Phase),
						"children inherit the select's phase")
				}
			}
		}
	}
	for _, n := range nodes {
		checkNode(n, known, 0, false)
	}

	g.validatePhaseCycles(ds, nodes, nodePos)
}

// validatePhaseCycles reports dependency cycles within one phase of one flow.
func (g *Gen) validatePhaseCycles(ds *diag.Diagnostics, nodes []Node, nodePos func(Node) token.Position) {
	for _, pl := range phaseLabels {
		var phase []Node
		for _, n := range nodes {
			if n.base().Phase == pl.phase {
				phase = append(phase, n)
			}
		}
		if len(phase) < 2 {
			continue
		}
		owner := map[string]int{}
		vars := map[string]bool{}
		for i, n := range phase {
			for _, d := range defines(n) {
				owner[d] = i
				vars[d] = true
			}
		}
		deps := make([][]int, len(phase))
		viaUses := map[[2]int]bool{}
		for i, n := range phase {
			declared := map[string]bool{}
			var collect func(m Node)
			collect = func(m Node) {
				for _, u := range m.base().Uses {
					declared[u] = true
				}
				if sel, ok := m.(*Select); ok {
					for ci := range sel.Cases {
						for _, child := range sel.Cases[ci].Body {
							collect(child)
						}
						for _, u := range sel.Cases[ci].Result.Uses {
							declared[u] = true
						}
					}
				}
			}
			collect(n)
			for _, u := range uses(n, vars) {
				j, ok := owner[u]
				if !ok || j == i {
					continue
				}
				deps[i] = append(deps[i], j)
				if declared[u] {
					viaUses[[2]int{i, j}] = true
				}
			}
		}
		const (
			white, gray, black = 0, 1, 2
		)
		color := make([]int, len(phase))
		var chain []int
		var cycle []int
		var visit func(i int) bool
		visit = func(i int) bool {
			color[i] = gray
			chain = append(chain, i)
			sorted := append([]int(nil), deps[i]...)
			sort.Ints(sorted)
			for _, j := range sorted {
				if color[j] == gray {
					start := 0
					for k, v := range chain {
						if v == j {
							start = k
							break
						}
					}
					cycle = append(append([]int(nil), chain[start:]...), j)
					return true
				}
				if color[j] == white && visit(j) {
					return true
				}
			}
			chain = chain[:len(chain)-1]
			color[i] = black
			return false
		}
		for i := range phase {
			if color[i] == white && visit(i) {
				break
			}
		}
		if cycle == nil {
			continue
		}
		var parts []string
		for k := 0; k < len(cycle); k++ {
			n := phase[cycle[k]]
			seg := fmt.Sprintf("%s %s", n.base().Origin.Directive, nodePos(n))
			if k+1 < len(cycle) && viaUses[[2]int{cycle[k], cycle[k+1]}] {
				seg += " (via Uses)"
			}
			parts = append(parts, seg)
		}
		ds.Error(nodePos(phase[cycle[0]]),
			fmt.Sprintf("dependency cycle in the %s section", pl.label),
			"cycle: "+strings.Join(parts, " -> "))
	}
}

// validateRawMetadata cross-checks a Raw node's declared Defines and
// Uses against its parsed lines.
func validateRawMetadata(ds *diag.Diagnostics, r *Raw, pos token.Position, known func(string) bool) {
	src := "package p\nfunc _() {\n" + strings.Join(r.Lines, "\n") + "\n}"
	f, err := parser.ParseFile(token.NewFileSet(), "", src, 0)
	if err != nil {
		// validateExprFields reports unparsable lines.
		return
	}
	body := f.Decls[0].(*ast.FuncDecl).Body
	declared := map[string]bool{}
	for _, st := range body.List {
		switch v := st.(type) {
		case *ast.AssignStmt:
			if v.Tok == token.DEFINE {
				for _, l := range v.Lhs {
					if id, ok := l.(*ast.Ident); ok {
						declared[id.Name] = true
					}
				}
			}
		case *ast.DeclStmt:
			if gd, ok := v.Decl.(*ast.GenDecl); ok && gd.Tok == token.VAR {
				for _, spec := range gd.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						for _, id := range vs.Names {
							declared[id.Name] = true
						}
					}
				}
			}
		}
	}
	free := map[string]bool{}
	sc := &scopeStack{}
	sc.push()
	walkStmts(body.List, sc, func(name string) { free[name] = true })
	for _, d := range r.Defines {
		if !declared[d] {
			ds.Error(pos, fmt.Sprintf("Raw declares Defines entry %q but its lines never define it", d),
				"Defines must list variables the lines declare")
		}
	}
	for _, u := range r.Uses {
		// Unknown Uses entries are reported separately.
		if !known(u) {
			continue
		}
		if !free[u] {
			ds.Error(pos, fmt.Sprintf("Raw declares Uses entry %q not used by its lines", u),
				"Uses must list variables the lines reference")
		}
	}
}
