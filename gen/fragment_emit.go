package gen

import (
	"bytes"
	"fmt"
	"go/types"
	"slices"
	"sort"
	"strings"

	"github.com/gofabrik/fabrik/diag"
)

// varInfo records type information needed by region signatures.
type varInfo struct {
	t        types.Type
	typeStr  string
	zero     string // declared zero for types the loader cannot resolve
	pathHint string // canonical path form, resolved lazily via LookupType
}

func (g *Gen) noteVarType(expr string, t types.Type) {
	if !isIdentifierExpr(expr) {
		return
	}
	if g.varTypes == nil {
		g.varTypes = map[string]varInfo{}
	}
	if v, ok := g.varTypes[expr]; ok && v.t != nil {
		return
	}
	// Defer rendering so recording a type does not register imports.
	g.varTypes[expr] = varInfo{t: t}
}

func (g *Gen) noteVarPathType(expr, path string) {
	if !isIdentifierExpr(expr) {
		return
	}
	if g.varTypes == nil {
		g.varTypes = map[string]varInfo{}
	}
	if _, ok := g.varTypes[expr]; ok {
		return
	}
	g.varTypes[expr] = varInfo{pathHint: path}
}

func isIdentifierExpr(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b == '_' || (i > 0 && b >= '0' && b <= '9') {
			continue
		}
		return false
	}
	return true
}

func (g *Gen) varZero(name string) string {
	v, ok := g.varTypes[name]
	if !ok {
		return "nil"
	}
	if v.zero != "" {
		return v.zero
	}
	if v.t == nil && v.pathHint != "" {
		v.t = g.typeFromPath(v.pathHint)
	}
	if v.t == nil && v.typeStr != "" {
		v.t = g.typeFromAlias(v.typeStr)
	}
	if v.t != nil {
		return zeroExpr(g, v.t)
	}
	if strings.HasPrefix(v.typeStr, "*") || strings.HasPrefix(v.typeStr, "[]") ||
		strings.HasPrefix(v.typeStr, "map[") || strings.HasPrefix(v.typeStr, "func") {
		return "nil"
	}
	if v.typeStr == "" {
		return "nil"
	}
	return v.typeStr + "{}"
}

func (g *Gen) varTypeStr(name string) (string, bool) {
	v, ok := g.varTypes[name]
	if !ok {
		return "", false
	}
	if v.typeStr == "" && v.t == nil && v.pathHint != "" {
		v.t = g.typeFromPath(v.pathHint)
	}
	if v.typeStr == "" && v.t != nil {
		v.typeStr = g.TypeExpr(v.t)
		g.varTypes[name] = v
	}
	if v.typeStr == "" {
		return "", false
	}
	return v.typeStr, true
}

// typeFromAlias resolves a rendered type like *alias.Name back to its
// type through the import table.
func (g *Gen) typeFromAlias(expr string) types.Type {
	stars := 0
	for strings.HasPrefix(expr, "*") {
		stars++
		expr = expr[1:]
	}
	alias, name, found := strings.CutLast(expr, ".")
	if !found || strings.ContainsAny(expr, "[]{} ") {
		return nil
	}
	for path, a := range g.imports {
		if a == alias {
			if t, ok := g.LookupType(path, name); ok {
				for ; stars > 0; stars-- {
					t = types.NewPointer(t)
				}
				return t
			}
		}
	}
	return nil
}

// typeFromPath resolves a canonical printed type path like
// *example.com/pkg.Name back to its type.
func (g *Gen) typeFromPath(path string) types.Type {
	stars := 0
	for strings.HasPrefix(path, "*") {
		stars++
		path = path[1:]
	}
	pkgPath, name, found := strings.CutLast(path, ".")
	if !found {
		return nil
	}
	t, ok := g.LookupType(pkgPath, name)
	if !ok {
		return nil
	}
	for ; stars > 0; stars-- {
		t = types.NewPointer(t)
	}
	return t
}

// DeclareVarType records a generated variable's type for region signatures.
// typeExpr must use this generator's import aliases.
func (g *Gen) DeclareVarType(varName, typeExpr, zero string) {
	if !isIdentifierExpr(varName) {
		return
	}
	if g.varTypes == nil {
		g.varTypes = map[string]varInfo{}
	}
	v := g.varTypes[varName]
	if v.typeStr == "" {
		v.typeStr = typeExpr
		v.zero = zero
		g.varTypes[varName] = v
	}
}

// SingletonAttribution attributes callback emissions to a singleton's demanding flows.
// The returned function restores the previous attribution.
func (g *Gen) SingletonAttribution(key string) func() {
	g.recordDemand(demandKey{kind: demandSingleton, key: key})
	return g.pushDemand(demandKey{kind: demandSingleton, key: key})
}

// flowNodes returns the node occurrences used by one flow.
func (g *Gen) flowNodes(flow string) []Node {
	var out []Node
	for _, sn := range g.frag.nodes {
		if slices.Contains(g.nodeUsage(sn), flow) {
			out = append(out, sn.n)
		}
	}
	return out
}

func (g *Gen) scopeFlowNodes(s *Scope) []Node {
	return g.flowNodes(s.fn)
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func (g *Gen) allFlows(usage []string) bool {
	total := len(g.scopes)
	n := 0
	for _, u := range usage {
		if u != "default" {
			n++
		}
	}
	return total > 0 && n == total
}

// regionConsumer identifies a variable use in a region or flow body.
type regionConsumer struct {
	flow string
	reg  *region
}

// analyzeRegions fills live values, cleanups, and context usage.
func (g *Gen) analyzeRegions(ps *planState, regions []*region) {
	st := g.frag
	var preludeOut string
	for _, reg := range regions {
		reg.liveIns, reg.liveOuts, reg.cleanups = nil, nil, nil
		reg.optsVar = ""
	}
	consumers := map[string][]regionConsumer{}
	addConsumer := func(v, flow string, reg *region) {
		consumers[v] = append(consumers[v], regionConsumer{flow, reg})
	}
	for j, sn := range st.nodes {
		for _, u := range uses(sn.n, ps.allVars) {
			for _, f := range ps.flowsOf[j] {
				addConsumer(u, f, ps.homeFor(j, f))
			}
		}
	}
	for _, s := range g.scopes {
		note := func(name string) {
			if ps.allVars[name] {
				addConsumer(name, s.fn, nil)
			}
		}
		for _, e := range s.rootExprs {
			if isIdentifierExpr(e) {
				note(e)
			} else {
				freeIdents(e, note)
			}
		}
		for _, c := range g.commandFuncs {
			if c.Scope == s {
				for _, e := range c.ValueExprs {
					freeIdents(e, note)
				}
			}
		}
	}
	for _, reg := range regions {
		memberSet := map[int]bool{}
		for _, m := range reg.members {
			memberSet[m] = true
		}
		seenIn := map[string]bool{}
		var memberNodes []Node
		for _, m := range reg.members {
			sn := st.nodes[m]
			memberNodes = append(memberNodes, sn.n)
			for _, u := range uses(sn.n, ps.allVars) {
				j, ok := ps.definedBy[u]
				if ok && !memberSet[j] && !seenIn[u] {
					seenIn[u] = true
					reg.liveIns = append(reg.liveIns, u)
				}
			}
			if c, ok := sn.n.(*Call); ok && c.Cleanup != "" {
				reg.cleanups = append(reg.cleanups, c.Cleanup)
			}
		}
		sort.SliceStable(reg.liveIns, func(a, b int) bool {
			return ps.definedBy[reg.liveIns[a]] < ps.definedBy[reg.liveIns[b]]
		})
		for _, m := range reg.members {
			for _, d := range defines(st.nodes[m].n) {
				for _, c := range consumers[d] {
					if reg.runs(c.flow) && c.reg != reg {
						reg.liveOuts = append(reg.liveOuts, d)
						break
					}
				}
			}
		}
		// The scope root a region produces is its primary result.
		if reg.anchorRoot != "" {
			for i, v := range reg.liveOuts {
				if v == reg.anchorRoot && i > 0 {
					copy(reg.liveOuts[1:i+1], reg.liveOuts[:i])
					reg.liveOuts[0] = v
				}
			}
		}
		reg.needsCtx = usesCtx(memberNodes, g.ctxVar)
	}
	// The prelude output is the first parameter and does not count toward the width limit.
	for _, reg := range regions {
		if reg.anchorPrelude && len(reg.liveOuts) == 1 {
			preludeOut = reg.liveOuts[0]
		}
	}
	if preludeOut != "" {
		for _, reg := range regions {
			for i, in := range reg.liveIns {
				if in == preludeOut {
					reg.optsVar = in
					copy(reg.liveIns[1:i+1], reg.liveIns[:i])
					reg.liveIns[0] = in
				}
			}
		}
	}
	g.regionConsumers = consumers
}

// typeCheckRegions verifies threaded values after the partition settles.
func (g *Gen) typeCheckRegions(regions []*region, ds *diag.Diagnostics) {
	st := g.frag
	for _, reg := range regions {
		vars := append([]string{}, reg.liveOuts...)
		vars = append(vars, reg.liveIns...)
		if reg.primaryVar != "" {
			vars = append(vars, reg.primaryVar)
		}
		for _, v := range vars {
			if _, ok := g.varTypeStr(v); !ok {
				ds.Error(st.nodes[reg.members[0]].n.base().Origin.Pos,
					fmt.Sprintf("internal: no recorded type for cross-region value %q", v), "")
			}
		}
	}
}

// regionNarrow enforces the signature width and discard limits.
func (g *Gen) regionNarrow(ps *planState, reg *region) bool {
	ins := len(reg.liveIns)
	if reg.optsVar != "" {
		ins--
	}
	outs := len(reg.liveOuts)
	if reg.primaryVar != "" {
		outs++
	}
	if ins > 4 || outs > 3 {
		return false
	}
	for _, f := range reg.usage {
		discarded := 0
		if reg.primaryVar != "" && !g.consumedIn(reg.primaryVar, f, reg) {
			discarded++
		}
		for _, v := range reg.liveOuts {
			if !g.consumedIn(v, f, reg) {
				discarded++
			}
		}
		if discarded > 1 {
			return false
		}
	}
	return true
}

func (g *Gen) consumedIn(v, flow string, self *region) bool {
	for _, c := range g.regionConsumers[v] {
		if c.flow == flow && c.reg != self {
			return true
		}
	}
	return false
}

// nameRegions derives names from stable content anchors before falling back to flow names.
func (g *Gen) nameRegions(st *fragmentStore, regions []*region) {
	// Reserve identifiers sharing the output package's declaration space.
	// Reservation is case-insensitive so split file names, which lowercase
	// the function name, stay one-to-one with the functions.
	taken := map[string]bool{"run": true, "main": true}
	for name := range g.outputIdents {
		taken[strings.ToLower(name)] = true
	}
	for alias := range g.aliasIdents {
		taken[strings.ToLower(alias)] = true
	}
	used := func(name string) bool { return taken[strings.ToLower(name)] }
	for _, reg := range regions {
		base, qualified := "", ""
		switch {
		case reg.anchorPrelude:
			base = "configOptions"
		case reg.primaryVar != "":
			ts, _ := g.varTypeStr(reg.primaryVar)
			base = "build" + upperFirst(typeBaseName(ts))
			qualified = "build" + typePkgName(ts) + upperFirst(typeBaseName(ts))
		case reg.anchorHook != "":
			fn := reg.anchorHook
			pkg := ""
			if before, after, found := strings.CutLast(fn, "."); found {
				pkg, fn = before, after
			}
			base = lowerFirst(fn)
			if pkg != "" {
				qualified = lowerFirst(fn) + upperFirst(pkg)
			}
		case reg.anchorProvider != "" && reg.providerNamesProduct():
			base = "build" + upperFirst(reg.anchorProvider)
			if reg.anchorProviderPkg != "" {
				qualified = "build" + reg.anchorProviderPkg + upperFirst(reg.anchorProvider)
			}
		case len(reg.liveOuts) > 0:
			ts, _ := g.varTypeStr(reg.liveOuts[0])
			base = "build" + upperFirst(typeBaseName(ts))
			qualified = "build" + typePkgName(ts) + upperFirst(typeBaseName(ts))
		case reg.anchorProvider != "":
			base = "build" + upperFirst(reg.anchorProvider)
			if reg.anchorProviderPkg != "" {
				qualified = "build" + reg.anchorProviderPkg + upperFirst(reg.anchorProvider)
			}
		case reg.anchorRoot != "":
			ts, _ := g.varTypeStr(reg.anchorRoot)
			base = "build" + upperFirst(typeBaseName(ts))
			qualified = "build" + typePkgName(ts) + upperFirst(typeBaseName(ts))
		default:
			base = g.flowSetName(reg.usage)
		}
		// Collisions qualify by package before falling back to ordinals.
		name := base
		if used(name) && qualified != "" && !used(qualified) {
			name = qualified
		}
		for n := 2; used(name); n++ {
			name = fmt.Sprintf("%s%d", base, n)
		}
		taken[strings.ToLower(name)] = true
		reg.fn = name
	}
}

// typePkgName returns the UpperCamel package qualifier of a rendered type.
func typePkgName(t string) string {
	t = strings.TrimLeft(t, "*")
	if before, _, found := strings.CutLast(t, "."); found {
		return upperFirst(before)
	}
	return ""
}

// flowSetName derives a fallback region name from its flows.
func (g *Gen) flowSetName(usage []string) string {
	sig := "Shared"
	if len(usage) == 1 {
		sig = upperFirst(strings.TrimPrefix(usage[0], "build"))
		if usage[0] == "default" {
			sig = "Default"
		}
	} else if !g.allFlows(usage) {
		var parts []string
		for _, f := range usage {
			parts = append(parts, upperFirst(strings.TrimPrefix(f, "build")))
		}
		sig = strings.Join(parts, "")
	}
	return "build" + sig
}

// defaultInlineNodes returns default-flow nodes not owned by a region.
func (g *Gen) defaultInlineNodes() []Node {
	var out []Node
	for i, sn := range g.frag.nodes {
		if g.fragPlan != nil && g.fragPlan.inRegion[i] {
			continue
		}
		u := g.nodeUsage(sn)
		if len(u) == 1 && u[0] == "default" {
			out = append(out, sn.n)
		}
	}
	return out
}

// regionEmitOrder orders regions by their first call across registered flows.
func (g *Gen) regionEmitOrder() []*region {
	seen := map[*region]bool{}
	var out []*region
	for _, s := range g.scopes {
		for _, step := range g.fragPlan.steps[s.fn] {
			if step.region != nil && !seen[step.region] {
				seen[step.region] = true
				out = append(out, step.region)
			}
		}
	}
	return out
}

func (g *Gen) writeRegionFuncs(b *bytes.Buffer) {
	for _, reg := range g.regionEmitOrder() {
		g.writeRegionFunc(b, reg)
	}
}

func (g *Gen) writeRegionFunc(b *bytes.Buffer, reg *region) {
	st := g.frag
	hasCleanup := len(reg.cleanups) > 0
	errsPkg := ""
	if hasCleanup {
		errsPkg = g.Import("errors")
	}
	var params []string
	if reg.needsCtx {
		params = append(params, g.ctxVar+" "+g.imports["context"]+".Context")
	}
	for _, in := range reg.liveIns {
		ts, _ := g.varTypeStr(in)
		params = append(params, in+" "+ts)
	}
	var results []string
	if reg.primaryVar != "" {
		ts, _ := g.varTypeStr(reg.primaryVar)
		results = append(results, ts)
	}
	for _, out := range reg.liveOuts {
		ts, _ := g.varTypeStr(out)
		results = append(results, ts)
	}
	if hasCleanup {
		results = append(results, "func() error")
	}
	results = append(results, "error")
	fmt.Fprintf(b, "\nfunc %s(%s) (%s) {\n", reg.fn, strings.Join(params, ", "), strings.Join(results, ", "))

	var memberNodes []Node
	for _, m := range reg.members {
		memberNodes = append(memberNodes, st.nodes[m].n)
	}
	if nodesHaveCheck(memberNodes) {
		b.WriteString("var err error\n")
	}
	hasErrSite := slices.ContainsFunc(memberNodes, nodeHasErrTail)
	ec := &errCtx{zeros: g.regionZeros(reg), unwind: hasCleanup && hasErrSite}

	type section struct {
		label    string
		clusters [][]phaseNode
	}
	var sections []section
	labeled := false
	universe := make([]Node, 0, len(st.nodes))
	for _, sn := range st.nodes {
		universe = append(universe, sn.n)
	}
	lc := newLayoutCtx(universe)
	// Cleanup composition must release in reverse emission order, which
	// the layout decides.
	var cleanups []string
	for _, pl := range phaseLabels {
		var nodes []phaseNode
		for i, m := range reg.members {
			if st.nodes[m].n.base().Phase == pl.phase {
				nodes = append(nodes, phaseNode{n: st.nodes[m].n, emit: i})
			}
		}
		if len(nodes) == 0 {
			continue
		}
		if len(nodes) > 1 {
			labeled = true
		}
		clusters := lc.layout(nodes)
		sections = append(sections, section{label: pl.label, clusters: clusters})
		for _, cluster := range clusters {
			for _, pn := range cluster {
				if c, ok := pn.n.(*Call); ok && c.Cleanup != "" {
					cleanups = append(cleanups, c.Cleanup)
				}
			}
		}
	}
	labeled = labeled && len(sections) > 1 && g.comments != CommentsOff
	if hasCleanup {
		b.WriteString("var " + strings.Join(cleanups, ", ") + " func() error\n")
		b.WriteString("cleanup := func() error {\n")
		b.WriteString("var errs []error\n")
		for _, cleanup := range slices.Backward(cleanups) {
			b.WriteString("if " + cleanup + " != nil {\n")
			b.WriteString("errs = append(errs, " + cleanup + "())\n")
			b.WriteString("}\n")
		}
		b.WriteString("return " + errsPkg + ".Join(errs...)\n")
		b.WriteString("}\n")
		if ec.unwind {
			b.WriteString("unwind := func(err error) error {\n")
			b.WriteString("return " + errsPkg + ".Join(err, cleanup())\n")
			b.WriteString("}\n")
		}
	}
	for si, sec := range sections {
		if si > 0 {
			b.WriteString("\n")
		}
		if labeled {
			b.WriteString("// " + sec.label + "\n")
		}
		for ci, cluster := range sec.clusters {
			if ci > 0 {
				b.WriteString("\n")
			}
			for _, pn := range cluster {
				g.nodeComments(b, pn.n)
				for _, line := range g.nodeLines(pn.n, ec) {
					b.WriteString(line)
					b.WriteString("\n")
				}
			}
		}
	}

	var rets []string
	if reg.primaryVar != "" {
		rets = append(rets, reg.primaryExpr)
	}
	rets = append(rets, reg.liveOuts...)
	if hasCleanup {
		rets = append(rets, "cleanup")
	}
	rets = append(rets, "nil")
	b.WriteString("\nreturn " + strings.Join(rets, ", ") + "\n}\n")
}

func (g *Gen) regionZeros(reg *region) string {
	var zs []string
	if reg.primaryVar != "" {
		zs = append(zs, g.varZero(reg.primaryVar))
	}
	for _, out := range reg.liveOuts {
		zs = append(zs, g.varZero(out))
	}
	if len(reg.cleanups) > 0 {
		zs = append(zs, "nil")
	}
	z := strings.Join(zs, ", ")
	if z != "" {
		z += ", "
	}
	return z
}

// writeRegionBody emits region calls, inline nodes, and the command epilogue.
func (g *Gen) writeRegionBody(b *bytes.Buffer, c CommandFunc) {
	s := c.Scope
	steps := g.fragPlan.steps[s.fn]
	cleanupN := 0
	errsPkg := ""
	for _, step := range steps {
		if step.region != nil && len(step.region.cleanups) > 0 {
			errsPkg = g.Import("errors")
		}
	}
	for _, step := range steps {
		if step.region == nil {
			n := g.frag.nodes[step.node].n
			g.nodeComments(b, n)
			for _, line := range g.nodeLines(n, nil) {
				b.WriteString(line)
				b.WriteString("\n")
			}
			continue
		}
		reg := step.region
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
		cleanupVar := ""
		if len(reg.cleanups) > 0 {
			cleanupN++
			cleanupVar = "cleanup"
			if cleanupN > 1 {
				cleanupVar = fmt.Sprintf("cleanup%d", cleanupN)
			}
			lhs = append(lhs, cleanupVar)
		}
		var args []string
		if reg.needsCtx {
			args = append(args, g.ctxVar)
		}
		args = append(args, reg.liveIns...)
		if len(lhs) == 0 {
			fmt.Fprintf(b, "if err := %s(%s); err != nil {\nreturn err\n}\n", reg.fn, strings.Join(args, ", "))
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
			// A short declaration needs one new name; err already exists as
			// the named return.
			assign = "="
		}
		fmt.Fprintf(b, "%s, err %s %s(%s)\n", strings.Join(lhs, ", "), assign, reg.fn, strings.Join(args, ", "))
		b.WriteString("if err != nil {\nreturn err\n}\n")
		if cleanupVar != "" {
			fmt.Fprintf(b, "defer func() {\nerr = %s.Join(err, %s())\n}()\n", errsPkg, cleanupVar)
		}
	}
	args := append([]string{"ctx"}, s.rootExprs...)
	args = append(args, c.ValueExprs...)
	fmt.Fprintf(b, "return %s(%s)\n", c.Fn, strings.Join(args, ", "))
}
