package gen

import (
	"go/token"
	"strings"
	"testing"
)

func layoutVars(t *testing.T, blocks [][]phaseNode) []string {
	t.Helper()
	var out []string
	for _, b := range blocks {
		var vars []string
		for _, pn := range b {
			d := defines(pn.n)
			if len(d) == 0 {
				vars = append(vars, "-")
				continue
			}
			vars = append(vars, d[0])
		}
		out = append(out, strings.Join(vars, " "))
	}
	return out
}

func call(v, fn string, args ...string) *Call {
	return &Call{Base: Base{Phase: PhaseWire}, Var: v, Fn: fn, Args: args}
}

func TestLayoutBatchOrderIsHard(t *testing.T) {
	a := &Call{Base: Base{Phase: PhaseRegister, Batch: "reg", Seq: 1}, Fn: "r.A"}
	b := &Call{Base: Base{Phase: PhaseRegister, Batch: "reg", Seq: 2}, Fn: "r.B"}
	c := &Call{Base: Base{Phase: PhaseRegister, Batch: "reg", Seq: 3}, Fn: "r.C"}
	// Batch sequence overrides conflicting anchors.
	c.Origin.Pos = token.Position{Filename: "a.go", Line: 1}
	a.Origin.Pos = token.Position{Filename: "z.go", Line: 9}
	universe := []Node{a, b, c}
	lc := newLayoutCtx(universe)
	blocks := lc.layout([]phaseNode{{n: a, emit: 0}, {n: b, emit: 1}, {n: c, emit: 2}})
	var got []string
	for _, blk := range blocks {
		for _, pn := range blk {
			got = append(got, pn.n.(*Call).Fn)
		}
	}
	if strings.Join(got, ",") != "r.A,r.B,r.C" {
		t.Fatalf("batch order not preserved: %v", got)
	}
	if len(blocks) != 1 {
		t.Fatalf("one batch must form one block, got %d", len(blocks))
	}
}

func TestLayoutHubDoesNotMergeItsConsumers(t *testing.T) {
	hub := call("db", "app.NewDB")
	var section []phaseNode
	universe := []Node{hub}
	section = append(section, phaseNode{n: hub, emit: 0})
	for i, name := range []string{"one", "two", "three", "four"} {
		n := call(name, "app.New"+name, "db")
		n.Origin.Pos = token.Position{Filename: name + ".go", Line: 1}
		universe = append(universe, n)
		section = append(section, phaseNode{n: n, emit: i + 1})
	}
	lc := newLayoutCtx(universe)
	blocks := lc.layout(section)
	// A high-fan-out value does not group otherwise unrelated consumers.
	if len(blocks) != 5 {
		t.Fatalf("hub consumers merged: %v", layoutVars(t, blocks))
	}
}

func TestLayoutExclusiveChainFormsOneBlock(t *testing.T) {
	a := call("store", "app.NewStore")
	b := call("cache", "app.NewCache", "store")
	c := call("svc", "app.NewSvc", "cache")
	a.Origin.Pos = token.Position{Filename: "a.go", Line: 1}
	other := call("other", "app.NewOther")
	other.Origin.Pos = token.Position{Filename: "zz.go", Line: 1}
	universe := []Node{a, b, c, other}
	lc := newLayoutCtx(universe)
	blocks := lc.layout([]phaseNode{{n: a, emit: 0}, {n: other, emit: 1}, {n: b, emit: 2}, {n: c, emit: 3}})
	got := layoutVars(t, blocks)
	if len(blocks) != 2 || got[0] != "store cache svc" || got[1] != "other" {
		t.Fatalf("exclusive chain must form one block apart from strangers: %v", got)
	}
}

func TestLayoutSharedDownstreamConsumerGroups(t *testing.T) {
	// Producers group when they reach the same downstream sink through different paths.
	src := call("sources", "app.Sources")
	stage := call("staged", "app.Stage", "sources")
	dial := call("dialect", "app.Dialect")
	db := call("mdb", "app.OpenDB")
	src.Origin.Pos = token.Position{Filename: "a.go", Line: 1}
	stage.Origin.Pos = token.Position{Filename: "s.go", Line: 5}
	dial.Origin.Pos = token.Position{Filename: "m.go", Line: 40}
	db.Origin.Pos = token.Position{Filename: "z.go", Line: 90}
	stranger := call("misc", "app.Misc")
	stranger.Origin.Pos = token.Position{Filename: "b.go", Line: 2}
	consumer := &Call{Base: Base{Phase: PhaseRegister}, Fn: "app.Migrate", Args: []string{"staged", "dialect", "mdb"}}
	universe := []Node{src, stage, stranger, dial, db, consumer}
	lc := newLayoutCtx(universe)
	// Grouping follows transitive sinks, not only immediate consumers.
	if lc.sharedConsumer(src, dial) != affSharedCons {
		t.Fatalf("transitive feature root not detected between sources and dialect")
	}
	blocks := lc.layout([]phaseNode{{n: src, emit: 0}, {n: stranger, emit: 1}, {n: stage, emit: 2}, {n: dial, emit: 3}, {n: db, emit: 4}})
	got := layoutVars(t, blocks)
	if got[0] != "sources staged dialect mdb" {
		t.Fatalf("transitive shared-consumer group wrong: %v", got)
	}
}

func TestLayoutBlockSizeCap(t *testing.T) {
	var universe []Node
	var section []phaseNode
	prev := ""
	for i := 0; i < layoutBlockMax+3; i++ {
		v := "v" + strings.Repeat("x", i+1)
		var n *Call
		if prev == "" {
			n = call(v, "app.New")
		} else {
			n = call(v, "app.New", prev)
		}
		universe = append(universe, n)
		section = append(section, phaseNode{n: n, emit: i})
		prev = v
	}
	lc := newLayoutCtx(universe)
	blocks := lc.layout(section)
	if len(blocks) < 2 {
		t.Fatalf("block cap not enforced: %d blocks", len(blocks))
	}
	if len(blocks[0]) != layoutBlockMax {
		t.Fatalf("first block size = %d, want the cap", len(blocks[0]))
	}
}

func TestLayoutExplicitGroupOverridesDistance(t *testing.T) {
	a := call("first", "app.A")
	b := call("second", "app.B")
	a.Group, b.Group = "feature", "feature"
	a.Origin.Pos = token.Position{Filename: "a.go", Line: 1}
	b.Origin.Pos = token.Position{Filename: "z.go", Line: 400}
	mid := call("mid", "app.Mid")
	mid.Origin.Pos = token.Position{Filename: "m.go", Line: 5}
	universe := []Node{a, mid, b}
	lc := newLayoutCtx(universe)
	blocks := lc.layout([]phaseNode{{n: a, emit: 0}, {n: mid, emit: 1}, {n: b, emit: 2}})
	got := layoutVars(t, blocks)
	if got[0] != "first second" {
		t.Fatalf("explicit group must hold together: %v", got)
	}
}

func TestLayoutUnrelatedAdditionKeepsNeighborhood(t *testing.T) {
	build := func(extra bool) []string {
		a := call("store", "app.NewStore")
		b := call("cache", "app.NewCache", "store")
		c := call("svc", "app.NewSvc", "cache")
		a.Origin.Pos = token.Position{Filename: "a.go", Line: 1}
		d := call("mailer", "app.NewMailer")
		d.Origin.Pos = token.Position{Filename: "m.go", Line: 1}
		universe := []Node{a, b, c, d}
		section := []phaseNode{{n: a, emit: 0}, {n: b, emit: 1}, {n: c, emit: 2}, {n: d, emit: 3}}
		if extra {
			// Insert a node whose anchor falls between existing blocks.
			e := call("metrics", "app.NewMetrics")
			e.Origin.Pos = token.Position{Filename: "ab.go", Line: 1}
			universe = append(universe, e)
			section = append(section, phaseNode{n: e, emit: 4})
		}
		lc := newLayoutCtx(universe)
		return layoutVars(t, lc.layout(section))
	}
	base := build(false)
	grown := build(true)
	// Existing blocks remain an ordered subsequence.
	j := 0
	for _, blk := range base {
		found := false
		for ; j < len(grown); j++ {
			if grown[j] == blk {
				found = true
				j++
				break
			}
		}
		if !found {
			t.Fatalf("existing blocks rearranged by an unrelated addition:\nbase %v\ngrown %v", base, grown)
		}
	}
}

func TestBeginBatchStampsTwoIndependentSequences(t *testing.T) {
	g := New()
	a1 := &Call{Base: Base{Phase: PhaseRegister}, Fn: "r.A1"}
	a2 := &Call{Base: Base{Phase: PhaseRegister}, Fn: "r.A2"}
	end := g.BeginBatch("alpha")
	g.Node(a1)
	g.Node(a2)
	end()
	b1 := &Call{Base: Base{Phase: PhaseRegister}, Fn: "q.B1"}
	b2 := &Call{Base: Base{Phase: PhaseRegister}, Fn: "q.B2"}
	end = g.BeginBatch("beta")
	g.Node(b1)
	g.Node(b2)
	end()
	if a1.Batch != "alpha" || a1.Seq != 1 || a2.Seq != 2 {
		t.Fatalf("alpha stamps wrong: %+v %+v", a1.Base, a2.Base)
	}
	if b1.Batch != "beta" || b1.Seq != 1 || b2.Seq != 2 {
		t.Fatalf("beta stamps wrong: %+v %+v", b1.Base, b2.Base)
	}
	// Each batch preserves its sequence despite conflicting anchors.
	a2.Origin.Pos = token.Position{Filename: "a.go", Line: 1}
	b2.Origin.Pos = token.Position{Filename: "b.go", Line: 1}
	universe := []Node{a1, a2, b1, b2}
	lc := newLayoutCtx(universe)
	blocks := lc.layout([]phaseNode{{n: b2, emit: 0}, {n: a2, emit: 1}, {n: b1, emit: 2}, {n: a1, emit: 3}})
	pos := map[string]int{}
	i := 0
	for _, blk := range blocks {
		for _, pn := range blk {
			pos[pn.n.(*Call).Fn] = i
			i++
		}
	}
	if pos["r.A1"] > pos["r.A2"] || pos["q.B1"] > pos["q.B2"] {
		t.Fatalf("batch order broken: %v", pos)
	}
	// Separate batches remain unordered relative to each other.
	if pos["q.B2"] > pos["r.A1"] {
		t.Fatalf("cross-batch ordering imposed: %v", pos)
	}
}
