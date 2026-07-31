package gen

import (
	"go/token"
	"sort"
	"strings"

	"github.com/gofabrik/fabrik/diag"
)

// Fragment emission records each demand once in a shared store. Connected,
// anchored nodes with the same flow usage become shared build functions;
// other nodes remain inline.

type storeNode struct {
	n      Node
	demand int            // index into demandOrder, -1 for tier-time emission
	flow   string         // flow active at emission, for demandless nodes
	pos    token.Position // nearest positioned materialization ancestor
}

type fragmentStore struct {
	nodes       []storeNode
	demandOrder []demandKey
	demandIndex map[demandKey]int
	active      []int            // demand stack during builds
	flow        string           // walking flow during scope walks
	noFlow      bool             // epilogue run: demands attribute through edges only
	posStack    []token.Position // positioned binds being materialized
}

func (st *fragmentStore) demandID(k demandKey) int {
	if i, ok := st.demandIndex[k]; ok {
		return i
	}
	if st.demandIndex == nil {
		st.demandIndex = map[demandKey]int{}
	}
	i := len(st.demandOrder)
	st.demandOrder = append(st.demandOrder, k)
	st.demandIndex[k] = i
	return i
}

func (st *fragmentStore) currentDemand() int {
	if len(st.active) == 0 {
		return -1
	}
	return st.active[len(st.active)-1]
}

// pushNodePos records a positioned materialization frame; positionless
// store nodes inherit the innermost frame for diagnostics.
func (g *Gen) pushNodePos(pos token.Position) func() {
	if g.frag == nil || !pos.IsValid() {
		return func() {}
	}
	g.frag.posStack = append(g.frag.posStack, pos)
	return func() { g.frag.posStack = g.frag.posStack[:len(g.frag.posStack)-1] }
}

func (st *fragmentStore) currentPos() token.Position {
	if len(st.posStack) == 0 {
		return token.Position{}
	}
	return st.posStack[len(st.posStack)-1]
}

// pushDemand activates a demand for node attribution during its build.
func (g *Gen) pushDemand(k demandKey) func() {
	if g.frag == nil {
		return func() {}
	}
	id := g.frag.demandID(k)
	g.frag.active = append(g.frag.active, id)
	return func() { g.frag.active = g.frag.active[:len(g.frag.active)-1] }
}

// WalkFlows materializes each scope against the shared store and then runs epilogues.
// Every demand executes at most once after all flow usage is known.
func (g *Gen) WalkFlows() diag.Diagnostics {
	var ds diag.Diagnostics
	for si, s := range g.scopes {
		g.frag.flow = s.fn
		for i, fn := range g.prologues {
			g.recordDemand(demandKey{kind: demandCallback, key: "prologue", ord: i})
			if si == 0 {
				done := g.pushDemand(demandKey{kind: demandCallback, key: "prologue", ord: i})
				// The validation pass reports prologue diagnostics; the walk
				// only emits code.
				fn()
				done()
			}
		}
		for _, root := range s.roots {
			expr, rds, ok := g.Instance(root.Type, root.Name)
			ds = append(ds, rds...)
			if !ok && len(rds) == 0 {
				msg, help := g.MissingBinding(root.Type, root.Name, func() (string, string) {
					return "no provider for " + g.TypeExpr(root.Type),
						"add a //fabrik:provider returning " + g.TypeExpr(root.Type)
				})
				ds.Error(s.pos, msg, help)
			}
			// Render root types only when an extracted signature needs their imports.
			s.rootExprs = append(s.rootExprs, expr)
		}
		for i := range g.epilogues {
			g.recordDemand(demandKey{kind: demandCallback, key: "epilogue", ord: i})
		}
	}
	g.frag.flow = ""
	// Epilogues inherit no flow; callbacks set singleton or config attribution.
	g.frag.noFlow = true
	for i, fn := range g.epilogues {
		ds = append(ds, g.reportCallbackDiags(i, fn())...)
	}
	g.frag.noFlow = false
	return ds
}

// nodeUsage returns the sorted flows that need a store node.
func (g *Gen) nodeUsage(sn storeNode) []string {
	if sn.demand >= 0 {
		return g.demandFlows(g.frag.demandOrder[sn.demand])
	}
	if sn.flow != "" {
		return []string{sn.flow}
	}
	return []string{"default"}
}

// region is an extracted build function for nodes with the same flow usage.
type region struct {
	usage    []string
	usageSet map[string]bool
	members  []int // store indices, ascending

	fn       string
	liveIns  []string
	liveOuts []string
	cleanups []string
	needsCtx bool
	optsVar  string // live-in produced by the prelude region, listed first

	// primaryExpr, when set, is a shared scope root assembled by the
	// region's return statement; callers bind it to primaryVar.
	primaryExpr string
	primaryVar  string

	anchorHook        string // hook callee, for naming
	anchorProvider    string // provider binding name, for naming
	anchorProviderPkg string // provider callee's package, for qualification
	anchorProviderVar string // variable the anchor provider defines
	anchorRoot        string // member-defined scope root variable
	anchorPrelude     bool
}

func (r *region) runs(flow string) bool { return r.usageSet[flow] }

// providerNamesProduct reports whether the anchor provider's own result
// is what callers receive; a provider consumed internally must not name
// the region after itself.
func (r *region) providerNamesProduct() bool {
	if len(r.liveOuts) == 0 {
		return true
	}
	for _, out := range r.liveOuts {
		if out == r.anchorProviderVar {
			return true
		}
	}
	return false
}

type planStep struct {
	region *region
	node   int // store index when region is nil
}

type regionPlan struct {
	regions  []*region
	steps    map[string][]planStep
	inRegion map[int]bool // store indices owned by any region
}

func usageKey(flows []string) string { return strings.Join(flows, "+") }

type planState struct {
	flowsOf   [][]string
	usage     []string
	isConfig  []bool
	definedBy map[string]int
	allVars   map[string]bool
	memberOf  map[int][]*region
	rootVars  map[string]bool // identifier scope roots
}

// homeFor returns the region that owns node i in the given flow.
func (ps *planState) homeFor(i int, flow string) *region {
	for _, r := range ps.memberOf[i] {
		if r.runs(flow) {
			return r
		}
	}
	return nil
}

// PlanFragments derives the region partition after WalkFlows and before rendering.
func (g *Gen) PlanFragments() diag.Diagnostics {
	g.propagateDemandUsage()
	return g.planRegions()
}

// planRegions extracts eligible connected components with identical usage and colocates configs.
func (g *Gen) planRegions() diag.Diagnostics {
	var ds diag.Diagnostics
	st := g.frag
	n := len(st.nodes)
	ps := &planState{
		flowsOf:   make([][]string, n),
		usage:     make([]string, n),
		isConfig:  make([]bool, n),
		definedBy: map[string]int{},
		allVars:   map[string]bool{},
		memberOf:  map[int][]*region{},
		rootVars:  map[string]bool{},
	}
	for _, s := range g.scopes {
		for _, e := range s.rootExprs {
			if isIdentifierExpr(e) {
				ps.rootVars[e] = true
			}
		}
	}
	for i, sn := range st.nodes {
		ps.flowsOf[i] = g.nodeUsage(sn)
		ps.usage[i] = usageKey(ps.flowsOf[i])
		_, ps.isConfig[i] = sn.n.(*ConfigLoad)
		for _, d := range defines(sn.n) {
			ps.definedBy[d] = i
			ps.allVars[d] = true
		}
		if c, ok := sn.n.(*Call); ok && c.Type != nil && c.Var != "" {
			g.noteVarType(c.Var, c.Type)
		}
	}

	regions := g.formComponents(ps)
	g.embedSharedRoots(ps, regions)
	g.colocateConfigs(ps)

	keep := func(pass func(*region) bool) {
		out := regions[:0]
		for _, reg := range regions {
			if pass(reg) {
				out = append(out, reg)
			} else {
				g.dissolve(ps, reg)
			}
		}
		regions = out
	}
	keep(func(reg *region) bool { return g.regionEligible(ps, reg, st) })

	// A cycle through a region means an outside node must emit between
	// its members in some flow; that region dissolves.
	for {
		broken := g.findUnschedulable(ps, regions)
		if broken == nil {
			break
		}
		g.dissolve(ps, broken)
		out := regions[:0]
		for _, reg := range regions {
			if reg != broken {
				out = append(out, reg)
			}
		}
		regions = out
	}

	// Dissolving a region changes neighboring liveness, so width checks iterate to a fixpoint.
	for {
		g.analyzeRegions(ps, regions)
		var wide *region
		for _, reg := range regions {
			if !g.regionNarrow(ps, reg) {
				wide = reg
				break
			}
		}
		if wide == nil {
			break
		}
		g.dissolve(ps, wide)
		out := regions[:0]
		for _, reg := range regions {
			if reg != wide {
				out = append(out, reg)
			}
		}
		regions = out
	}

	g.typeCheckRegions(regions, &ds)
	g.nameRegions(st, regions)
	inRegion := map[int]bool{}
	for _, reg := range regions {
		for _, m := range reg.members {
			inRegion[m] = true
		}
	}
	steps := map[string][]planStep{}
	for _, s := range g.scopes {
		var out []planStep
		g.orderFlow(ps, s.fn, &out)
		steps[s.fn] = out
	}
	g.fragPlan = &regionPlan{regions: regions, steps: steps, inRegion: inRegion}
	return ds
}

// formComponents finds connected non-config components with identical usage.
func (g *Gen) formComponents(ps *planState) []*region {
	st := g.frag
	n := len(st.nodes)
	parent := make([]int, n)
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	for i, sn := range st.nodes {
		if ps.isConfig[i] {
			continue
		}
		for _, u := range uses(sn.n, ps.allVars) {
			if j, ok := ps.definedBy[u]; ok && !ps.isConfig[j] && ps.usage[j] == ps.usage[i] {
				parent[find(i)] = find(j)
			}
		}
	}
	// A shared compound root connects the components producing its
	// inputs: the assembled value is the region's product.
	rootFlows := map[string][]string{}
	for _, s := range g.scopes {
		for _, e := range s.rootExprs {
			if e == "" || isIdentifierExpr(e) {
				continue
			}
			rootFlows[e] = append(rootFlows[e], s.fn)
		}
	}
	for e, flows := range rootFlows {
		if len(flows) < 2 {
			continue
		}
		sort.Strings(flows)
		key := usageKey(flows)
		first := -1
		freeIdents(e, func(name string) {
			j, ok := ps.definedBy[name]
			if !ok || ps.isConfig[j] || ps.usage[j] != key {
				return
			}
			if first < 0 {
				first = j
				return
			}
			parent[find(first)] = find(j)
		})
	}
	comps := map[int][]int{}
	var order []int
	for i := range st.nodes {
		if ps.isConfig[i] {
			continue
		}
		r := find(i)
		if comps[r] == nil {
			order = append(order, r)
		}
		comps[r] = append(comps[r], i)
	}
	var regions []*region
	for _, r := range order {
		members := comps[r]
		sort.Ints(members)
		reg := &region{usage: ps.flowsOf[members[0]], members: members, usageSet: map[string]bool{}}
		for _, f := range reg.usage {
			reg.usageSet[f] = true
		}
		regions = append(regions, reg)
		for _, m := range members {
			ps.memberOf[m] = append(ps.memberOf[m], reg)
		}
	}
	return regions
}

// embedSharedRoots moves a shared compound root into the region defining its inputs.
func (g *Gen) embedSharedRoots(ps *planState, regions []*region) {
	type rootUse struct {
		flows []string
		root  ScopeRoot
		at    []*Scope
		idx   []int
	}
	byExpr := map[string]*rootUse{}
	var order []string
	for _, s := range g.scopes {
		for i, e := range s.rootExprs {
			if e == "" || isIdentifierExpr(e) {
				continue
			}
			ru := byExpr[e]
			if ru == nil {
				ru = &rootUse{root: s.roots[i]}
				byExpr[e] = ru
				order = append(order, e)
			}
			ru.flows = append(ru.flows, s.fn)
			ru.at = append(ru.at, s)
			ru.idx = append(ru.idx, i)
		}
	}
	for _, e := range order {
		ru := byExpr[e]
		if len(ru.flows) < 2 {
			continue
		}
		var target *region
		ok, refs := true, false
		freeIdents(e, func(name string) {
			if !ps.allVars[name] {
				return
			}
			refs = true
			rs := ps.memberOf[ps.definedBy[name]]
			if len(rs) != 1 || (target != nil && target != rs[0]) {
				ok = false
				return
			}
			target = rs[0]
		})
		if !ok || !refs || target == nil || target.primaryExpr != "" {
			continue
		}
		sort.Strings(ru.flows)
		if usageKey(ru.flows) != usageKey(target.usage) {
			continue
		}
		typ := g.TypeExpr(ru.root.Type)
		v := g.takeIdent(lowerFirst(typeBaseName(typ)))
		target.primaryExpr = e
		target.primaryVar = v
		// Callers bind the assembled value; consumer analysis must see
		// epilogue references to it.
		ps.allVars[v] = true
		g.DeclareVarType(v, typ, zeroExpr(g, ru.root.Type))
		for i, s := range ru.at {
			s.rootExprs[ru.idx[i]] = v
		}
	}
}

// typeBaseName extracts the bare type name from a rendered type.
func typeBaseName(t string) string {
	t = strings.TrimLeft(t, "*")
	if i := strings.LastIndexByte(t, '.'); i >= 0 {
		t = t[i+1:]
	}
	if t == "" || !isIdentifierExpr(t) {
		return "root"
	}
	return t
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// colocateConfigs embeds a load only when every consuming flow agrees on its region.
func (g *Gen) colocateConfigs(ps *planState) {
	st := g.frag
	rootRefs := map[string]map[string]bool{}
	for _, s := range g.scopes {
		refs := map[string]bool{}
		for _, e := range s.rootExprs {
			if isIdentifierExpr(e) {
				refs[e] = true
				continue
			}
			freeIdents(e, func(name string) {
				if ps.allVars[name] {
					refs[name] = true
				}
			})
		}
		rootRefs[s.fn] = refs
	}
	for i, sn := range st.nodes {
		if !ps.isConfig[i] {
			continue
		}
		v := sn.n.(*ConfigLoad).Var
		perFlow := map[string]map[*region]bool{}
		note := func(flow string, r *region) {
			m := perFlow[flow]
			if m == nil {
				m = map[*region]bool{}
				perFlow[flow] = m
			}
			m[r] = true
		}
		for j, other := range st.nodes {
			if j == i {
				continue
			}
			for _, u := range uses(other.n, ps.allVars) {
				if u != v {
					continue
				}
				for _, f := range ps.flowsOf[j] {
					note(f, ps.homeFor(j, f))
				}
			}
		}
		for _, f := range ps.flowsOf[i] {
			if rootRefs[f][v] {
				note(f, nil)
			}
		}
		agreed := true
		for f, homes := range perFlow {
			if len(homes) != 1 {
				agreed = false
				_ = f
				break
			}
		}
		if !agreed {
			continue
		}
		seen := map[*region]bool{}
		for _, homes := range perFlow {
			for r := range homes {
				if r != nil && !seen[r] {
					seen[r] = true
					r.members = append(r.members, i)
					sort.Ints(r.members)
					ps.memberOf[i] = append(ps.memberOf[i], r)
				}
			}
		}
	}
}

// regionEligible requires multiple flows, multiple nodes, and a content anchor.
func (g *Gen) regionEligible(ps *planState, reg *region, st *fragmentStore) bool {
	if len(reg.usage) < 2 {
		return false
	}
	if len(reg.members) < 2 {
		return false
	}
	return g.regionAnchored(ps, reg, st)
}

// regionAnchored reports whether a region contains a stable naming anchor.
func (g *Gen) regionAnchored(ps *planState, reg *region, st *fragmentStore) bool {
	if reg.primaryExpr != "" {
		return true
	}
	anchored := false
	for _, m := range reg.members {
		sn := st.nodes[m]
		for _, d := range defines(sn.n) {
			if ps.rootVars[d] && reg.anchorRoot == "" {
				reg.anchorRoot = d
				anchored = true
			}
		}
		if sn.n.base().Phase == PhaseSetup {
			if c, ok := sn.n.(*Call); ok && reg.anchorHook == "" {
				reg.anchorHook = c.Fn
			}
			anchored = true
			continue
		}
		if sn.demand < 0 {
			continue
		}
		k := st.demandOrder[sn.demand]
		switch k.kind {
		case demandType:
			if k.name != "" {
				if reg.anchorProvider == "" {
					reg.anchorProvider = k.name
					if ds := defines(sn.n); len(ds) > 0 {
						reg.anchorProviderVar = ds[0]
					}
					if c, ok := sn.n.(*Call); ok {
						if i := strings.IndexByte(c.Fn, '.'); i > 0 {
							reg.anchorProviderPkg = upperFirst(c.Fn[:i])
						}
					}
				}
				anchored = true
			}
		case demandSingleton:
			// The ":prelude" singleton key anchors and names the config-source region.
			if strings.HasSuffix(k.key, ":prelude") {
				reg.anchorPrelude = true
			}
			anchored = true
		}
	}
	return anchored
}

func (g *Gen) dissolve(ps *planState, reg *region) {
	for _, m := range reg.members {
		rs := ps.memberOf[m]
		out := rs[:0]
		for _, r := range rs {
			if r != reg {
				out = append(out, r)
			}
		}
		if len(out) == 0 {
			delete(ps.memberOf, m)
		} else {
			ps.memberOf[m] = out
		}
	}
	if reg.primaryVar != "" {
		for _, s := range g.scopes {
			for i, e := range s.rootExprs {
				if e == reg.primaryVar {
					s.rootExprs[i] = reg.primaryExpr
				}
			}
		}
		reg.primaryExpr, reg.primaryVar = "", ""
	}
	reg.members = nil
}

// findUnschedulable reports a region whose members cannot stay
// contiguous in some flow, or nil.
func (g *Gen) findUnschedulable(ps *planState, regions []*region) *region {
	for _, s := range g.scopes {
		if broken := g.orderFlow(ps, s.fn, nil); broken != nil {
			return broken
		}
	}
	return nil
}

// orderFlow orders regions and inline nodes by dependencies, hook barriers, and store order.
func (g *Gen) orderFlow(ps *planState, flow string, out *[]planStep) *region {
	st := g.frag
	type unit struct {
		reg   *region
		nodes []int
		first int
		hook  bool
		prov  bool
	}
	var units []*unit
	regUnit := map[*region]*unit{}
	nodeUnit := map[int]*unit{}
	for i := range st.nodes {
		inFlow := false
		for _, f := range ps.flowsOf[i] {
			if f == flow {
				inFlow = true
				break
			}
		}
		if !inFlow {
			continue
		}
		if r := ps.homeFor(i, flow); r != nil {
			u := regUnit[r]
			if u == nil {
				u = &unit{reg: r, first: i}
				regUnit[r] = u
				units = append(units, u)
			}
			u.nodes = append(u.nodes, i)
			nodeUnit[i] = u
		} else {
			u := &unit{nodes: []int{i}, first: i}
			units = append(units, u)
			nodeUnit[i] = u
		}
	}
	for _, u := range units {
		for _, i := range u.nodes {
			switch st.nodes[i].n.base().Phase {
			case PhaseSetup:
				u.hook = true
			case PhaseWire, PhaseMiddleware, PhaseRegister:
				u.prov = true
			}
		}
	}
	deps := map[*unit]map[*unit]bool{}
	for _, u := range units {
		deps[u] = map[*unit]bool{}
		for _, i := range u.nodes {
			for _, use := range uses(st.nodes[i].n, ps.allVars) {
				j, ok := ps.definedBy[use]
				if !ok {
					continue
				}
				du := nodeUnit[j]
				if du == nil {
					// Values absent from this flow arrive through region live-outs.
					if r := ps.homeFor(j, flow); r != nil {
						du = regUnit[r]
					}
				}
				if du != nil && du != u {
					deps[u][du] = true
				}
			}
		}
	}
	sort.SliceStable(units, func(a, b int) bool { return units[a].first < units[b].first })
	emitted := map[*unit]bool{}
	hooksLeft := 0
	for _, u := range units {
		if u.hook {
			hooksLeft++
		}
	}
	remaining := len(units)
	for remaining > 0 {
		progress := false
		for _, u := range units {
			if emitted[u] {
				continue
			}
			ready := true
			for d := range deps[u] {
				if !emitted[d] {
					ready = false
					break
				}
			}
			if !ready || (u.prov && !u.hook && hooksLeft > 0) {
				continue
			}
			emitted[u] = true
			remaining--
			if u.hook {
				hooksLeft--
			}
			if out != nil {
				if u.reg != nil {
					*out = append(*out, planStep{region: u.reg})
				} else {
					*out = append(*out, planStep{node: u.nodes[0]})
				}
			}
			progress = true
			break
		}
		if !progress {
			for _, u := range units {
				if !emitted[u] && u.reg != nil {
					return u.reg
				}
			}
			// A cycle with no region involved was already reported by the
			// validation pass; emit nothing further for this flow.
			return nil
		}
	}
	return nil
}
