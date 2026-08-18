package directive

import (
	"go/token"
	"go/types"
	"strings"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

func mw(name string, global bool, file string, line int, opt ...func(*mwNode)) *mwNode {
	nd := &mwNode{
		name:   name,
		global: global,
		fn:     strings.ToUpper(name[:1]) + name[1:],
		pos:    token.Position{Filename: file, Line: line},
	}
	for _, fn := range opt {
		fn(nd)
	}
	return nd
}

func requires(names ...string) func(*mwNode) {
	return func(nd *mwNode) {
		for _, n := range names {
			nd.requires = append(nd.requires, mwRef{name: n, pos: nd.pos})
		}
	}
}

func after(names ...string) func(*mwNode) {
	return func(nd *mwNode) {
		for _, n := range names {
			nd.after = append(nd.after, mwRef{name: n, pos: nd.pos})
		}
	}
}

func before(names ...string) func(*mwNode) {
	return func(nd *mwNode) {
		for _, n := range names {
			nd.before = append(nd.before, mwRef{name: n, pos: nd.pos})
		}
	}
}

func outer(nd *mwNode) { nd.beforeAll = true }
func inner(nd *mwNode) { nd.afterAll = true }

func index(decls []*mwNode) map[string]*mwNode {
	byName := map[string]*mwNode{}
	for _, nd := range decls {
		if nd.name != "" {
			byName[nd.name] = nd
		}
	}
	return byName
}

func stackNames(t *testing.T, globals []*mwNode, byName map[string]*mwNode) ([]string, map[string]string, orderResult, diag.Diagnostics) {
	t.Helper()
	decls := append([]*mwNode(nil), globals...)
	seen := map[*mwNode]bool{}
	for _, nd := range globals {
		seen[nd] = true
	}
	for _, nd := range byName {
		if !seen[nd] {
			decls = append(decls, nd)
		}
	}
	labels := mwLabels(decls)
	res, ds := resolveOrder(globals, decls, byName, labels)
	var names []string
	reasons := map[string]string{}
	for _, e := range res.stack {
		l := labels[e.nd]
		names = append(names, l)
		reasons[l] = e.label
	}
	return names, reasons, res, ds
}

func wantOrder(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("stack order = %v, want %v", got, want)
	}
}

func wantError(t *testing.T, ds diag.Diagnostics, substr string) {
	t.Helper()
	for _, d := range ds {
		if d.Severity == diag.SevError && strings.Contains(d.Message, substr) {
			return
		}
	}
	t.Fatalf("no error containing %q in %v", substr, ds)
}

func wantNoDiags(t *testing.T, ds diag.Diagnostics) {
	t.Helper()
	if len(ds) != 0 {
		t.Fatalf("unexpected diagnostics: %v", ds)
	}
}

func TestOrderFallbackIsFileLine(t *testing.T) {
	a := mw("a", true, "b.go", 5)
	b := mw("b", true, "a.go", 9)
	c := mw("c", true, "a.go", 3)
	names, reasons, _, ds := stackNames(t, []*mwNode{a, b, c}, index([]*mwNode{a, b, c}))
	wantNoDiags(t, ds)
	wantOrder(t, names, []string{"c", "b", "a"})
	if reasons["c"] != "unconstrained (file order)" {
		t.Fatalf("reason = %q", reasons["c"])
	}
}

func TestOrderEdgesAndRequires(t *testing.T) {
	session := mw("session", true, "m.go", 10)
	auth := mw("sessionauth", true, "a.go", 1, requires("session"))
	names, reasons, _, ds := stackNames(t, []*mwNode{session, auth}, index([]*mwNode{session, auth}))
	wantNoDiags(t, ds)
	wantOrder(t, names, []string{"session", "sessionauth"})
	if !strings.Contains(reasons["sessionauth"], "requires=session") {
		t.Fatalf("reason = %q", reasons["sessionauth"])
	}
}

func TestOrderBands(t *testing.T) {
	logger := mw("logger", true, "z.go", 99, outer)
	tail := mw("tail", true, "a.go", 1, inner)
	mid := mw("mid", true, "m.go", 5)
	names, reasons, _, ds := stackNames(t, []*mwNode{logger, tail, mid}, index([]*mwNode{logger, tail, mid}))
	wantNoDiags(t, ds)
	wantOrder(t, names, []string{"logger", "mid", "tail"})
	if reasons["logger"] != "before=*" || reasons["tail"] != "after=*" {
		t.Fatalf("band reasons = %q, %q", reasons["logger"], reasons["tail"])
	}
}

func TestOrderCrossBandSatisfiedKeepsReason(t *testing.T) {
	logger := mw("logger", true, "a.go", 1, outer)
	session := mw("session", true, "b.go", 1, after("logger"))
	names, reasons, _, ds := stackNames(t, []*mwNode{logger, session}, index([]*mwNode{logger, session}))
	wantNoDiags(t, ds)
	wantOrder(t, names, []string{"logger", "session"})
	if !strings.Contains(reasons["session"], "after=logger") {
		t.Fatalf("satisfied cross-band edge dropped from reasons: %q", reasons["session"])
	}
}

func TestOrderCrossBandContradictionErrors(t *testing.T) {
	inn := mw("tail", true, "a.go", 1, inner)
	mid := mw("mid", true, "b.go", 1, after("tail"))
	_, _, _, ds := stackNames(t, []*mwNode{inn, mid}, index([]*mwNode{inn, mid}))
	wantError(t, ds, "contradicts")
}

func TestOrderCrossBandHardContradictionErrors(t *testing.T) {
	// Hard dependencies cannot contradict wildcard band order.
	tail := mw("tail", true, "a.go", 1, inner)
	mid := mw("mid", true, "b.go", 1, requires("tail"))
	_, _, _, ds := stackNames(t, []*mwNode{tail, mid}, index([]*mwNode{tail, mid}))
	wantError(t, ds, "contradicts")
}

func TestOrderSoftCrossBandContradictionErrors(t *testing.T) {
	out := mw("head", true, "a.go", 1, outer)
	mid := mw("mid", true, "b.go", 1, before("head"))
	_, _, _, ds := stackNames(t, []*mwNode{out, mid}, index([]*mwNode{out, mid}))
	wantError(t, ds, "contradicts")
}

func TestOrderSoftAbsentTargetIsInactive(t *testing.T) {
	a := mw("a", true, "a.go", 1, after("phantom"))
	names, reasons, res, ds := stackNames(t, []*mwNode{a}, index([]*mwNode{a}))
	wantNoDiags(t, ds)
	wantOrder(t, names, []string{"a"})
	if len(res.inactive) != 1 || res.inactive[0].opt != "after=phantom" {
		t.Fatalf("inactive edges = %+v", res.inactive)
	}
	if !strings.Contains(reasons["a"], "after=phantom (inactive)") {
		t.Fatalf("inactive edge invisible in reason: %q", reasons["a"])
	}
}

func TestOrderSoftNonGlobalTargetErrors(t *testing.T) {
	route := mw("guard", false, "r.go", 1)
	g := mw("g", true, "g.go", 1, after("guard"))
	_, _, _, ds := stackNames(t, []*mwNode{g}, index([]*mwNode{route, g}))
	wantError(t, ds, "not global")
}

func TestOrderUnnamedGlobalCarriesEdges(t *testing.T) {
	session := mw("session", true, "s.go", 1)
	anon := mw("x", true, "a.go", 1, requires("session"))
	anon.name = ""
	names, _, _, ds := stackNames(t, []*mwNode{anon, session}, index([]*mwNode{session}))
	wantNoDiags(t, ds)
	wantOrder(t, names, []string{"session", "X"})
}

func TestOrderPerBandCycleErrors(t *testing.T) {
	a := mw("a", true, "m.go", 10, after("b"))
	b := mw("b", true, "m.go", 20, after("a"))
	_, _, _, ds := stackNames(t, []*mwNode{a, b}, index([]*mwNode{a, b}))
	wantError(t, ds, "cycle")
}

func TestOrderRequiresOnlyCycleLeftToValidate(t *testing.T) {
	// Pure requires cycles are reported only by validateDecls.
	a := mw("a", true, "m.go", 10, requires("b"))
	b := mw("b", true, "m.go", 20, requires("a"))
	decls := []*mwNode{a, b}
	_, _, _, ds := stackNames(t, decls, index(decls))
	wantNoDiags(t, ds)
	vds := validateDecls(decls, index(decls), mwLabels(decls))
	wantError(t, vds, "cycle")
}

func TestOrderIndependentCycleStillReports(t *testing.T) {
	// Validation and resolution each report their independent cycle.
	a := mw("a", true, "m.go", 10, requires("b"))
	b := mw("b", true, "m.go", 20, requires("a"))
	g1 := mw("g1", true, "m.go", 30, after("g2"))
	g2 := mw("g2", true, "m.go", 40, after("g1"))
	decls := []*mwNode{a, b, g1, g2}
	_, _, _, ds := stackNames(t, decls, index(decls))
	ds = append(ds, validateDecls(decls, index(decls), mwLabels(decls))...)
	cycles := 0
	for _, d := range ds {
		if d.Severity == diag.SevError && strings.Contains(d.Message, "cycle") {
			cycles++
		}
	}
	if cycles != 2 {
		t.Fatalf("cycle errors = %d, want exactly 2 in %v", cycles, ds)
	}
}

func TestValidateDeclsReportsAllRequiresCycles(t *testing.T) {
	a := mw("a", false, "m.go", 10, requires("b"))
	b := mw("b", false, "m.go", 20, requires("a"))
	c := mw("c", false, "m.go", 30, requires("d"))
	d := mw("d", false, "m.go", 40, requires("c"))
	decls := []*mwNode{a, b, c, d}
	ds := validateDecls(decls, index(decls), mwLabels(decls))
	cycles := 0
	for _, dg := range ds {
		if dg.Severity == diag.SevError && strings.Contains(dg.Message, "cycle") {
			cycles++
		}
	}
	if cycles != 2 {
		t.Fatalf("independent requires= cycles reported = %d, want 2 in %v", cycles, ds)
	}
}

func TestOrderStackStaysCompleteUnderCycle(t *testing.T) {
	// Cycle detection backtracks past dead ends without dropping globals from the stack.
	x := mw("x", true, "m.go", 10, after("b"))
	sink := mw("sink", true, "m.go", 20, after("x"))
	b := mw("b", true, "m.go", 30, after("x"))
	decls := []*mwNode{x, sink, b}
	names, _, _, ds := stackNames(t, decls, index(decls))
	wantError(t, ds, "cycle")
	if len(names) != 3 {
		t.Fatalf("stack lost members under a cycle: %v", names)
	}
}

func TestValidateDeclsCycleRemovalTerminates(t *testing.T) {
	// Dependencies into a removed cycle do not make validation rediscover it.
	a := mw("a", false, "m.go", 10, requires("b"))
	b := mw("b", false, "m.go", 20, requires("a"))
	c := mw("c", false, "m.go", 30, requires("a"))
	c.used, a.used, b.used = true, true, true
	decls := []*mwNode{a, b, c}
	done := make(chan diag.Diagnostics, 1)
	go func() { done <- validateDecls(decls, index(decls), mwLabels(decls)) }()
	select {
	case ds := <-done:
		cycles := 0
		for _, d := range ds {
			if d.Severity == diag.SevError && strings.Contains(d.Message, "cycle") {
				cycles++
			}
		}
		if cycles != 1 {
			t.Fatalf("cycle errors = %d, want exactly 1 in %v", cycles, ds)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("validateDecls did not terminate")
	}
}

func TestOrderOneNonRequiresCycleAcrossBands(t *testing.T) {
	o1 := mw("o1", true, "m.go", 10, outer, after("o2"))
	o2 := mw("o2", true, "m.go", 20, outer, after("o1"))
	m1 := mw("m1", true, "m.go", 30, after("m2"))
	m2 := mw("m2", true, "m.go", 40, after("m1"))
	decls := []*mwNode{o1, o2, m1, m2}
	_, _, _, ds := stackNames(t, decls, index(decls))
	cycles := 0
	for _, d := range ds {
		if d.Severity == diag.SevError && strings.Contains(d.Message, "cycle") {
			cycles++
		}
	}
	if cycles != 1 {
		t.Fatalf("resolver cycle errors = %d, want exactly 1 in %v", cycles, ds)
	}
}

func TestOrderTargetedNodeIsNotUnconstrained(t *testing.T) {
	session := mw("session", true, "s.go", 1)
	auth := mw("sessionauth", true, "a.go", 1, requires("session"))
	_, reasons, _, ds := stackNames(t, []*mwNode{session, auth}, index([]*mwNode{session, auth}))
	wantNoDiags(t, ds)
	if reasons["session"] != "before sessionauth" {
		t.Fatalf("targeted node reason = %q, want \"before sessionauth\"", reasons["session"])
	}
}

func TestRequiresCycleDiagPointsAtRef(t *testing.T) {
	a := mw("a", false, "m.go", 10)
	a.requires = []mwRef{{name: "b", pos: token.Position{Filename: "m.go", Line: 10, Column: 42}}}
	b := mw("b", false, "m.go", 20, requires("a"))
	decls := []*mwNode{a, b}
	ds := validateDecls(decls, index(decls), mwLabels(decls))
	for _, d := range ds {
		if strings.Contains(d.Message, "cycle") {
			if d.Pos.Column != 42 {
				t.Fatalf("cycle diagnostic at %v, want the requires= ref position (col 42)", d.Pos)
			}
			return
		}
	}
	t.Fatal("no cycle diagnostic")
}

func TestValidateDeclsFindings(t *testing.T) {
	missing := mw("m1", true, "a.go", 1, requires("ghost"))
	glb := mw("g", true, "a.go", 2, requires("routed"))
	routed := mw("routed", false, "a.go", 3)
	routed.used = true
	unref := mw("unused", false, "a.go", 4)
	bare := mw("x", false, "a.go", 5)
	bare.name = ""
	decls := []*mwNode{missing, glb, routed, unref, bare}
	ds := validateDecls(decls, index(decls), mwLabels(decls))
	wantError(t, ds, "unknown middleware")
	wantError(t, ds, "not global")
	wantError(t, ds, "inert")
	var warns []string
	for _, d := range ds {
		if d.Severity == diag.SevWarning {
			warns = append(warns, d.Message)
		}
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "never referenced") {
		t.Fatalf("warnings = %v, want only the unreferenced one", warns)
	}
}

func TestLabelsDisambiguateOnCollision(t *testing.T) {
	a := mw("x", true, "/abs/checkout/pkga/mw.go", 3)
	b := mw("x", true, "/abs/checkout/pkgb/mw.go", 7)
	a.name, b.name = "", ""
	a.fn, b.fn = "LogAndRecover", "LogAndRecover"
	a.pkg = types.NewPackage("example.com/app/pkga", "mwpkg")
	b.pkg = types.NewPackage("example.com/app/pkgb", "mwpkg")
	labels := mwLabels([]*mwNode{a, b})
	if labels[a] == labels[b] {
		t.Fatalf("colliding labels: %q vs %q", labels[a], labels[b])
	}
	if labels[a] != "mwpkg.LogAndRecover (mw.go:3)" || labels[b] != "mwpkg.LogAndRecover (mw.go:7)" {
		t.Fatalf("collision format = %q, %q; want pkg.Fn (file:line) with a base filename", labels[a], labels[b])
	}
	if strings.Contains(labels[a], "/abs/") {
		t.Fatalf("label leaks the checkout path: %q", labels[a])
	}

	// Import paths disambiguate identical package, file, and line labels.
	c := mw("x", true, "/abs/checkout/pkga/mw.go", 3)
	d := mw("x", true, "/abs/checkout/pkgb/mw.go", 3)
	c.name, d.name = "", ""
	c.fn, d.fn = "LogAndRecover", "LogAndRecover"
	c.pkg = types.NewPackage("example.com/app/pkga", "mwpkg")
	d.pkg = types.NewPackage("example.com/app/pkgb", "mwpkg")
	labels = mwLabels([]*mwNode{c, d})
	if labels[c] == labels[d] {
		t.Fatalf("same base file and line collided: %q vs %q", labels[c], labels[d])
	}
	if labels[c] != "mwpkg.LogAndRecover (example.com/app/pkga/mw.go:3)" {
		t.Fatalf("import-path disambiguation = %q", labels[c])
	}
	if strings.Contains(labels[c], "/abs/") || strings.Contains(labels[d], "/abs/") {
		t.Fatalf("label leaks the checkout path: %q %q", labels[c], labels[d])
	}
}

func TestOrderDeterministicAcrossRuns(t *testing.T) {
	build := func() ([]string, string) {
		a := mw("a", true, "a.go", 1, after("c", "b"))
		b := mw("b", true, "b.go", 1)
		c := mw("c", true, "c.go", 1)
		decls := []*mwNode{a, b, c}
		names, reasons, _, _ := stackNames(t, decls, index(decls))
		return names, reasons["a"]
	}
	n1, r1 := build()
	for range 20 {
		n2, r2 := build()
		if strings.Join(n1, ",") != strings.Join(n2, ",") || r1 != r2 {
			t.Fatalf("nondeterministic: %v %q vs %v %q", n1, r1, n2, r2)
		}
	}
	if r1 != "after=b; after=c" {
		t.Fatalf("multi-reason label not sorted: %q", r1)
	}
}

func TestOrderMixedCycleOverlappingRequiresCycleNotReported(t *testing.T) {
	// Resolver diagnostics do not duplicate an overlapping requires cycle.
	a := mw("a", true, "m.go", 10, requires("b"))
	b := mw("b", true, "m.go", 20, requires("a"))
	c := mw("c", true, "m.go", 30, after("a"), before("b"))
	decls := []*mwNode{a, b, c}
	_, _, _, ds := stackNames(t, decls, index(decls))
	ds = append(ds, validateDecls(decls, index(decls), mwLabels(decls))...)
	cycles := 0
	for _, d := range ds {
		if d.Severity == diag.SevError && strings.Contains(d.Message, "cycle") {
			cycles++
		}
	}
	if cycles != 1 {
		t.Fatalf("cycle errors = %d, want exactly 1 in %v", cycles, ds)
	}
}

func TestOrderCrossBandRequiresCycleCondemnsAcrossBands(t *testing.T) {
	// An overlapping soft cycle is suppressed when its node belongs to a requires cycle.
	a := mw("a", true, "m.go", 10, requires("b"), after("c"))
	b := mw("b", true, "m.go", 20, inner, requires("a"))
	c := mw("c", true, "m.go", 30, after("a"))
	decls := []*mwNode{a, b, c}
	_, _, _, ds := stackNames(t, decls, index(decls))
	ds = append(ds, validateDecls(decls, index(decls), mwLabels(decls))...)
	softCycles, reqCycles := 0, 0
	for _, d := range ds {
		if d.Severity != diag.SevError {
			continue
		}
		switch {
		case strings.Contains(d.Message, "requires= cycle"):
			reqCycles++
		case strings.Contains(d.Message, "cycle"):
			softCycles++
		}
	}
	if reqCycles != 1 || softCycles != 0 {
		t.Fatalf("requires cycles = %d (want 1), overlapping soft cycles = %d (want 0) in %v", reqCycles, softCycles, ds)
	}
}

func TestGraphSectionPayload(t *testing.T) {
	logger := mw("logger", true, "m.go", 10, outer)
	session := mw("session", true, "m.go", 20, requires("logger"))
	auth := mw("auth", false, "m.go", 30)
	auth.used = true

	m := &Middleware{
		byName:  map[string]*mwNode{"logger": logger, "session": session, "auth": auth},
		globals: []*mwNode{logger, session},
		decls:   []*mwNode{logger, session, auth},
		chains: []resolvedChain{
			{route: "GET /admin", names: []string{"auth"}},
		},
	}
	session.after = append(session.after, mwRef{name: "missing", pos: token.Position{Filename: "m.go", Line: 21}})

	g := gen.New()
	m.graphSection(g)

	gr := g.Graph()
	if gr.Sections == nil {
		t.Fatal("Sections is nil after graphSection")
	}
	section, ok := gr.Sections["middleware"]
	if !ok {
		t.Fatal("middleware section not registered")
	}
	mwSection, ok := section.(MWGraphSection)
	if !ok {
		t.Fatalf("section type = %T, want MWGraphSection", section)
	}
	if len(mwSection.Stack) != 2 {
		t.Fatalf("stack length = %d, want 2", len(mwSection.Stack))
	}
	if mwSection.Stack[0].Name != "logger" {
		t.Fatalf("stack[0].Name = %q, want logger", mwSection.Stack[0].Name)
	}
	if mwSection.Stack[1].Name != "session" {
		t.Fatalf("stack[1].Name = %q, want session", mwSection.Stack[1].Name)
	}
	if len(mwSection.Chains) != 1 || mwSection.Chains[0].Route != "GET /admin" {
		t.Fatalf("chains = %+v, want one GET /admin chain", mwSection.Chains)
	}
	if len(mwSection.Inactive) != 1 || mwSection.Inactive[0].Ref != "after=missing" {
		t.Fatalf("inactive = %+v, want after=missing", mwSection.Inactive)
	}
}

func TestOrderCrossLayerRequiresCycleCondemns(t *testing.T) {
	// Requires cycles containing route middleware also suppress overlapping soft-cycle diagnostics.
	a := mw("a", true, "m.go", 10, requires("b"), after("c"))
	b := mw("b", false, "m.go", 20, requires("a"))
	b.used = true
	c := mw("c", true, "m.go", 30, after("a"))
	decls := []*mwNode{a, b, c}
	_, _, _, ds := stackNames(t, []*mwNode{a, c}, index(decls))
	ds = append(ds, validateDecls(decls, index(decls), mwLabels(decls))...)
	softCycles, reqCycles := 0, 0
	for _, d := range ds {
		if d.Severity != diag.SevError {
			continue
		}
		switch {
		case strings.Contains(d.Message, "requires= cycle"):
			reqCycles++
		case strings.Contains(d.Message, "cycle"):
			softCycles++
		}
	}
	if reqCycles != 1 || softCycles != 0 {
		t.Fatalf("requires cycles = %d (want 1), overlapping soft cycles = %d (want 0) in %v", reqCycles, softCycles, ds)
	}
}
