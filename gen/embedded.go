package gen

import (
	"bytes"
	"fmt"
	"go/token"
	"sort"
	"strings"

	"github.com/gofabrik/fabrik/diag"
)

// commandPathKey is the canonical full command path: space-joined
// segments, matching fabrik.yaml entrypoints.
func commandPathKey(c CommandFunc) string {
	path := c.Path
	if len(path) == 0 {
		path = []string{c.Name}
	}
	return strings.Join(path, " ")
}

// SelectCommands keeps the named entrypoints, or every command when paths is empty.
func (g *Gen) SelectCommands(paths []string, pos map[string]token.Position) diag.Diagnostics {
	if len(paths) == 0 {
		return nil
	}
	var ds diag.Diagnostics
	known := map[string]int{}
	for i, c := range g.commandFuncs {
		known[commandPathKey(c)] = i
	}
	selected := map[int]bool{}
	for _, p := range paths {
		i, ok := known[p]
		if !ok {
			names := make([]string, 0, len(known))
			for k := range known {
				names = append(names, k)
			}
			sort.Strings(names)
			ds.Error(pos[p], fmt.Sprintf("unknown entrypoint %q", p), "known: "+strings.Join(names, ", "))
			continue
		}
		selected[i] = true
	}
	if ds.HasFatal() {
		return ds
	}
	keepScope := map[*Scope]bool{}
	var cf []CommandFunc
	for i, c := range g.commandFuncs {
		if selected[i] {
			cf = append(cf, c)
			keepScope[c.Scope] = true
		}
	}
	var scopes []*Scope
	for _, s := range g.scopes {
		if keepScope[s] {
			scopes = append(scopes, s)
		}
	}
	g.commandFuncs, g.scopes = cf, scopes
	return ds
}

// mainFileName is the primary generated file: main.gen.go beside
// package main, fabrik.gen.go inside an embedded package.
func (g *Gen) mainFileName() string {
	if g.embedded {
		return "fabrik.gen.go"
	}
	return "main.gen.go"
}

func (g *Gen) entrypointsNeedErrors() bool {
	if g.fragPlan == nil {
		return false
	}
	for _, c := range g.commandFuncs {
		for _, step := range g.fragPlan.steps[c.Scope.fn] {
			if step.region != nil && len(step.region.cleanups) > 0 {
				return true
			}
			if step.region == nil {
				if call, ok := g.frag.nodes[step.node].n.(*Call); ok && call.Cleanup != "" {
					return true
				}
			}
		}
	}
	return false
}

// writeEntrypoints renders each command as an exported constructor that returns its roots and cleanup.
func (g *Gen) writeEntrypoints(b *bytes.Buffer, ctxPkg string) {
	taken := map[string]bool{}
	for name := range g.outputIdents {
		taken[name] = true
	}
	for _, c := range g.commandFuncs {
		path := c.Path
		if len(path) == 0 {
			path = []string{c.Name}
		}
		base := "New"
		for _, seg := range path {
			base += exportSegment(seg)
		}
		name := base
		for n := 2; taken[name]; n++ {
			name = fmt.Sprintf("%s%d", base, n)
		}
		taken[name] = true
		g.writeEntrypoint(b, c, name, ctxPkg)
	}
}

func exportSegment(seg string) string {
	var out strings.Builder
	for _, part := range strings.Split(seg, "-") {
		out.WriteString(upperFirst(part))
	}
	return out.String()
}

// nodeCanFail reports whether a node's rendering has an error path;
// an entry holding cleanups must unwind through every such path.
func nodeCanFail(n Node) bool {
	switch n := n.(type) {
	case *Call:
		return n.Err != ErrNone
	case *ConfigLoad:
		return true
	case *Select:
		return true
	case *Raw:
		return n.Check
	}
	return false
}

func (g *Gen) writeEntrypoint(b *bytes.Buffer, c CommandFunc, name, ctxPkg string) {
	s := c.Scope
	steps := g.fragPlan.steps[s.fn]

	// Cleanup slots unwind in reverse emission order.
	var cleanups []string
	regionCleanup := map[*region]string{}
	for _, step := range steps {
		if step.region != nil {
			if len(step.region.cleanups) > 0 {
				v := fmt.Sprintf("cleanup%d", len(regionCleanup)+1)
				regionCleanup[step.region] = v
				cleanups = append(cleanups, v)
			}
			continue
		}
		if call, ok := g.frag.nodes[step.node].n.(*Call); ok && call.Cleanup != "" {
			cleanups = append(cleanups, call.Cleanup)
		}
	}
	hasCleanup := len(cleanups) > 0
	errsPkg := ""
	if hasCleanup {
		errsPkg = g.Import("errors")
	}

	var results []string
	var zeros []string
	for _, root := range s.roots {
		results = append(results, g.TypeExpr(root.Type))
		zeros = append(zeros, zeroExpr(g, root.Type))
	}
	if hasCleanup {
		results = append(results, "func() error")
		zeros = append(zeros, "nil")
	}
	results = append(results, "error")
	zeroPrefix := strings.Join(zeros, ", ")
	if zeroPrefix != "" {
		zeroPrefix += ", "
	}

	fmt.Fprintf(b, "\nfunc %s(%s %s.Context) (%s) {\n", name, g.ctxVar, ctxPkg, strings.Join(results, ", "))

	// Predeclare err only for statements that cannot declare it themselves.
	regionLHS := map[*region][]string{}
	needErr := false
	canFail := false
	var inline []Node
	for _, step := range steps {
		if step.region != nil {
			reg := step.region
			canFail = true
			var lhs []string
			if reg.primaryVar != "" {
				if g.consumedIn(reg.primaryVar, s.fn, reg) {
					lhs = append(lhs, reg.primaryVar)
				} else {
					lhs = append(lhs, "_")
				}
			}
			for _, out := range reg.liveOuts {
				if g.consumedIn(out, s.fn, reg) {
					lhs = append(lhs, out)
				} else {
					lhs = append(lhs, "_")
				}
			}
			if v, ok := regionCleanup[reg]; ok {
				lhs = append(lhs, v)
			}
			regionLHS[reg] = lhs
			allBlank := len(lhs) > 0
			for _, l := range lhs {
				if l != "_" {
					allBlank = false
					break
				}
			}
			if allBlank {
				needErr = true
			}
			continue
		}
		n := g.frag.nodes[step.node].n
		inline = append(inline, n)
		if nodeCanFail(n) {
			canFail = true
		}
	}
	if nodesHaveCheck(inline) {
		needErr = true
	}
	if needErr {
		b.WriteString("var err error\n")
	}
	ec := &errCtx{zeros: zeroPrefix, unwind: hasCleanup && canFail}
	if hasCleanup {
		b.WriteString("var " + strings.Join(cleanups, ", ") + " func() error\n")
		if ec.unwind {
			b.WriteString("unwind := func(err error) error {\n")
			for _, line := range unwindLines(cleanups, errsPkg) {
				b.WriteString(line)
				b.WriteString("\n")
			}
			b.WriteString("return err\n")
			b.WriteString("}\n")
		}
	}

	for _, step := range steps {
		if step.region == nil {
			n := g.frag.nodes[step.node].n
			g.nodeComments(b, n)
			for _, line := range g.nodeLines(n, ec) {
				b.WriteString(line)
				b.WriteString("\n")
			}
			continue
		}
		reg := step.region
		lhs := regionLHS[reg]
		var args []string
		if reg.needsCtx {
			args = append(args, g.ctxVar)
		}
		args = append(args, reg.liveIns...)
		call := fmt.Sprintf("%s(%s)", reg.fn, strings.Join(args, ", "))
		if len(lhs) == 0 {
			for _, line := range ec.check("err := " + call + "; err != nil") {
				b.WriteString(line)
				b.WriteString("\n")
			}
			continue
		}
		assign := ":="
		bound := false
		for _, l := range lhs {
			if l != "_" {
				bound = true
				break
			}
		}
		if !bound {
			assign = "="
		}
		fmt.Fprintf(b, "%s, err %s %s\n", strings.Join(lhs, ", "), assign, call)
		for _, line := range ec.errReturn() {
			b.WriteString(line)
			b.WriteString("\n")
		}
	}

	rets := append([]string{}, s.rootExprs...)
	if hasCleanup {
		b.WriteString("\ncleanup := func() error {\n")
		b.WriteString("var errs []error\n")
		for i := len(cleanups) - 1; i >= 0; i-- {
			b.WriteString("if " + cleanups[i] + " != nil {\n")
			b.WriteString("errs = append(errs, " + cleanups[i] + "())\n")
			b.WriteString("}\n")
		}
		b.WriteString("return " + errsPkg + ".Join(errs...)\n")
		b.WriteString("}\n")
		rets = append(rets, "cleanup")
	}
	rets = append(rets, "nil")
	b.WriteString("return " + strings.Join(rets, ", ") + "\n}\n")
}
