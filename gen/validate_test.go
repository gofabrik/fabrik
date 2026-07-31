package gen

import (
	"go/token"
	"strings"
	"testing"
)

func vpos(file string, line int) token.Position {
	return token.Position{Filename: file, Line: line, Column: 1}
}

func TestValidateGraphDuplicateDefines(t *testing.T) {
	g := New()
	g.SetDirective("provider")
	g.Node(&Assign{Base: Base{Phase: PhaseWire, Origin: Origin{Pos: vpos("a.go", 3)}}, Var: "x", Expr: "1"})
	g.Node(&Assign{Base: Base{Phase: PhaseWire, Origin: Origin{Pos: vpos("b.go", 7)}}, Var: "x", Expr: "2"})
	ds := g.ValidateGraph()
	if len(ds) != 1 || !strings.Contains(ds[0].Message, `defined twice`) {
		t.Fatalf("diagnostics = %v, want one duplicate-define error", ds)
	}
	if ds[0].Pos.Filename != "b.go" {
		t.Errorf("position = %v, want the second definition", ds[0].Pos)
	}
	if !strings.Contains(ds[0].Help, "a.go:3") {
		t.Errorf("help = %q, want the first definition's position", ds[0].Help)
	}
}

func TestValidateGraphUnknownUses(t *testing.T) {
	g := New()
	g.SetDirective("assets")
	g.Node(&Raw{Base: Base{Phase: PhaseWire, Uses: []string{"ghost"}, Origin: Origin{Pos: vpos("a.go", 4)}}, Lines: []string{"_ = 1"}})
	ds := g.ValidateGraph()
	if len(ds) != 1 || !strings.Contains(ds[0].Message, `"ghost"`) {
		t.Fatalf("diagnostics = %v, want an unknown-uses error", ds)
	}
}

func TestValidateGraphCycle(t *testing.T) {
	g := New()
	g.SetDirective("assets")
	g.Node(&Raw{Base: Base{Phase: PhaseWire, Uses: []string{"b"}, Origin: Origin{Pos: vpos("a.go", 3), Directive: "assets"}}, Defines: []string{"a"}, Lines: []string{"a := b"}})
	g.Node(&Raw{Base: Base{Phase: PhaseWire, Uses: []string{"a"}, Origin: Origin{Pos: vpos("a.go", 9), Directive: "assets"}}, Defines: []string{"b"}, Lines: []string{"b := a"}})
	ds := g.ValidateGraph()
	if len(ds) != 1 {
		t.Fatalf("diagnostics = %v, want one cycle error", ds)
	}
	msg := ds[0].Message + " " + ds[0].Help
	for _, want := range []string{"cycle", "a.go:3", "a.go:9", "assets", "Uses"} {
		if !strings.Contains(msg, want) {
			t.Errorf("cycle diagnostic misses %q: %s", want, msg)
		}
	}
}

func TestValidateGraphRawMetadata(t *testing.T) {
	g := New()
	g.SetDirective("config")
	g.Node(&Raw{Base: Base{Phase: PhaseConfig, Origin: Origin{Pos: vpos("c.go", 2)}},
		Defines: []string{"declared", "phantom"},
		Lines:   []string{"declared := load()"}})
	ds := g.ValidateGraph()
	if len(ds) != 1 || !strings.Contains(ds[0].Message, `"phantom"`) {
		t.Fatalf("diagnostics = %v, want the undeclared define reported", ds)
	}
}

func TestValidateGraphRawUsesNotFree(t *testing.T) {
	g := New()
	g.SetDirective("config")
	g.Node(&Assign{Base: Base{Phase: PhaseWire}, Var: "real", Expr: "1"})
	g.Node(&Raw{Base: Base{Phase: PhaseWire, Uses: []string{"real"}, Origin: Origin{Pos: vpos("c.go", 5)}},
		Lines: []string{"_ = 2"}})
	ds := g.ValidateGraph()
	if len(ds) != 1 || !strings.Contains(ds[0].Message, `"real"`) || !strings.Contains(ds[0].Message, "not used") {
		t.Fatalf("diagnostics = %v, want the stale Uses entry reported", ds)
	}
}

func TestValidateGraphCleanupCollision(t *testing.T) {
	g := New()
	g.SetDirective("provider")
	g.Node(&Call{Base: Base{Phase: PhaseWire, Origin: Origin{Pos: vpos("p.go", 3)}}, Var: "a", Fn: "newA", Cleanup: "closeIt"})
	g.Node(&Call{Base: Base{Phase: PhaseWire, Origin: Origin{Pos: vpos("p.go", 9)}}, Var: "b", Fn: "newB", Cleanup: "closeIt"})
	ds := g.ValidateGraph()
	if len(ds) != 1 || !strings.Contains(ds[0].Message, "closeIt") {
		t.Fatalf("diagnostics = %v, want the cleanup collision", ds)
	}
}

func TestValidateGraphChildPhaseMismatch(t *testing.T) {
	g := New()
	g.SetDirective("provider:select")
	sel := &Select{
		Base: Base{Phase: PhaseWire, Origin: Origin{Pos: vpos("s.go", 3)}},
		Var:  "v", Iface: "x.I", KeyExpr: `"k"`, FmtPkg: "fmt",
		Cases: []Case{{
			Value:  "one",
			Body:   []Node{&Assign{Base: Base{Phase: PhaseRegister, Origin: Origin{Pos: vpos("s.go", 5)}}, Var: "w", Expr: "1"}},
			Result: Call{Base: Base{}, Var: "r", Fn: "mk"},
		}},
	}
	g.Node(sel)
	ds := g.ValidateGraph()
	if len(ds) != 1 || !strings.Contains(ds[0].Message, "phase") {
		t.Fatalf("diagnostics = %v, want one child-phase mismatch (the unset result must have inherited)", ds)
	}
}

func TestSelectChildrenInheritPhase(t *testing.T) {
	g := New()
	g.SetDirective("provider:select")
	sel := &Select{
		Base: Base{Phase: PhaseWire},
		Var:  "v", Iface: "x.I", KeyExpr: `"k"`, FmtPkg: "fmt",
		Cases: []Case{{
			Value:  "one",
			Body:   []Node{&Assign{Var: "w", Expr: "1"}},
			Result: Call{Var: "r", Fn: "mk"},
		}},
	}
	g.Node(sel)
	if got := sel.Cases[0].Result.Base.Phase; got != PhaseWire {
		t.Fatalf("result phase = %v, want inherited PhaseWire", got)
	}
	if got := sel.Cases[0].Body[0].base().Phase; got != PhaseWire {
		t.Fatalf("body child phase = %v, want inherited PhaseWire", got)
	}
	if ds := g.ValidateGraph(); len(ds) != 0 {
		t.Fatalf("diagnostics = %v, want none after inheritance", ds)
	}
}

func TestValidateGraphScopeFallbackPosition(t *testing.T) {
	g := New()
	g.SetDirective("provider")
	g.AddScope("buildX", vpos("cmd.go", 12))
	g.frag.flow = "buildX"
	g.Node(&Assign{Base: Base{Phase: PhaseWire}, Var: "x", Expr: "1"})
	g.Node(&Assign{Base: Base{Phase: PhaseWire}, Var: "x", Expr: "2"})
	g.frag.flow = ""
	ds := g.ValidateGraph()
	if len(ds) != 1 {
		t.Fatalf("diagnostics = %v, want one", ds)
	}
	if ds[0].Pos.Filename != "cmd.go" {
		t.Errorf("position = %v, want the scope's declaration as fallback", ds[0].Pos)
	}
}

func TestValidateGraphCaseLocalUsesAndDuplicates(t *testing.T) {
	g := New()
	g.SetDirective("provider:select")
	sel := &Select{
		Base: Base{Phase: PhaseWire, Origin: Origin{Pos: vpos("s.go", 3)}},
		Var:  "v", Iface: "x.I", KeyExpr: `"k"`, FmtPkg: "fmt",
		Cases: []Case{{
			Value: "one",
			Body: []Node{
				&Assign{Var: "local", Expr: "1"},
				&Raw{Base: Base{Uses: []string{"local"}}, Lines: []string{"_ = local"}},
				&Assign{Base: Base{Origin: Origin{Pos: vpos("s.go", 8)}}, Var: "local", Expr: "2"},
			},
			Result: Call{Var: "r", Fn: "mk"},
		}},
	}
	g.Node(sel)
	ds := g.ValidateGraph()
	if len(ds) != 1 || !strings.Contains(ds[0].Message, "defined twice in one case") {
		t.Fatalf("diagnostics = %v, want only the in-case duplicate (case-local Uses resolve)", ds)
	}
}

func TestValidateGraphResultPhaseMismatch(t *testing.T) {
	g := New()
	g.SetDirective("provider:select")
	sel := &Select{
		Base: Base{Phase: PhaseWire, Origin: Origin{Pos: vpos("s.go", 3)}},
		Var:  "v", Iface: "x.I", KeyExpr: `"k"`, FmtPkg: "fmt",
		Cases: []Case{{
			Value:  "one",
			Result: Call{Base: Base{Phase: PhaseRegister, Origin: Origin{Pos: vpos("s.go", 6)}}, Var: "r", Fn: "mk"},
		}},
	}
	g.Node(sel)
	ds := g.ValidateGraph()
	if len(ds) != 1 || !strings.Contains(ds[0].Message, "case result phase") {
		t.Fatalf("diagnostics = %v, want the explicit result phase mismatch", ds)
	}
}

func TestValidateGraphNestedSelectRawMetadata(t *testing.T) {
	g := New()
	g.SetDirective("provider:select")
	inner := &Select{
		Base: Base{Phase: PhaseWire},
		Var:  "iv", Iface: "x.J", KeyExpr: `"k2"`, FmtPkg: "fmt",
		Cases: []Case{{
			Value: "deep",
			Body: []Node{&Raw{Base: Base{Origin: Origin{Pos: vpos("n.go", 9)}},
				Defines: []string{"missing"}, Lines: []string{"_ = 1"}}},
			Result: Call{Var: "ir", Fn: "mkInner"},
		}},
	}
	sel := &Select{
		Base: Base{Phase: PhaseWire, Origin: Origin{Pos: vpos("n.go", 3)}},
		Var:  "v", Iface: "x.I", KeyExpr: `"k"`, FmtPkg: "fmt",
		Cases: []Case{{
			Value:  "one",
			Body:   []Node{inner},
			Result: Call{Var: "r", Fn: "mk"},
		}},
	}
	g.Node(sel)
	ds := g.ValidateGraph()
	if len(ds) != 1 || !strings.Contains(ds[0].Message, `"missing"`) {
		t.Fatalf("diagnostics = %v, want the nested Raw's undeclared define", ds)
	}
}

func TestValidateGraphRawFreeIdentsAreScopeAware(t *testing.T) {
	g := New()
	g.SetDirective("config")
	g.Node(&Assign{Base: Base{Phase: PhaseWire}, Var: "real", Expr: "1"})
	// Selectors, composite keys, and shadowed locals are not free uses.
	g.Node(&Raw{Base: Base{Phase: PhaseWire, Uses: []string{"real"}, Origin: Origin{Pos: vpos("c.go", 5)}},
		Lines: []string{
			"x := pkg.real",
			"_ = T{real: 1}",
			"func() { real := 2; _ = real }()",
			"_ = x",
		}})
	ds := g.ValidateGraph()
	if len(ds) != 1 || !strings.Contains(ds[0].Message, "not used") {
		t.Fatalf("diagnostics = %v, want the stale Uses entry despite lookalike identifiers", ds)
	}
}

func TestValidateGraphRawSelfDefineIsNotAUse(t *testing.T) {
	g := New()
	g.SetDirective("config")
	g.Node(&Assign{Base: Base{Phase: PhaseWire}, Var: "x", Expr: "1"})
	// A Raw-local declaration makes the flow variable's Uses entry stale.
	g.Node(&Raw{Base: Base{Phase: PhaseWire, Uses: []string{"x"}, Origin: Origin{Pos: vpos("c.go", 5)}},
		Defines: []string{"y"},
		Lines:   []string{"y := 1", "x := y", "_ = x"}})
	ds := g.ValidateGraph()
	if len(ds) != 1 || !strings.Contains(ds[0].Message, "not used") {
		t.Fatalf("diagnostics = %v, want the self-defined lookalike rejected", ds)
	}
}

func TestValidateGraphResultUsesUnknown(t *testing.T) {
	g := New()
	g.SetDirective("provider:select")
	sel := &Select{
		Base: Base{Phase: PhaseWire, Origin: Origin{Pos: vpos("s.go", 3)}},
		Var:  "v", Iface: "x.I", KeyExpr: `"k"`, FmtPkg: "fmt",
		Cases: []Case{{
			Value:  "one",
			Result: Call{Base: Base{Uses: []string{"ghost"}, Origin: Origin{Pos: vpos("s.go", 6)}}, Var: "r", Fn: "mk"},
		}},
	}
	g.Node(sel)
	ds := g.ValidateGraph()
	if len(ds) != 1 || !strings.Contains(ds[0].Message, `"ghost"`) {
		t.Fatalf("diagnostics = %v, want the result's unknown Uses entry", ds)
	}
}

func TestValidateGraphCycleAttributionFromNestedUses(t *testing.T) {
	g := New()
	g.SetDirective("provider:select")
	sel := &Select{
		Base: Base{Phase: PhaseWire, Origin: Origin{Pos: vpos("a.go", 3), Directive: "provider:select"}},
		Var:  "a", Iface: "x.I", KeyExpr: `"k"`, FmtPkg: "fmt",
		Cases: []Case{{
			Value: "one",
			Body: []Node{&Raw{Base: Base{Uses: []string{"b"}, Origin: Origin{Pos: vpos("a.go", 5)}},
				Lines: []string{"_ = b"}}},
			Result: Call{Var: "r", Fn: "mk"},
		}},
	}
	g.Node(sel)
	g.Node(&Assign{Base: Base{Phase: PhaseWire, Origin: Origin{Pos: vpos("a.go", 9), Directive: "web"}}, Var: "b", Expr: "a"})
	ds := g.ValidateGraph()
	if len(ds) != 1 {
		t.Fatalf("diagnostics = %v, want the cycle", ds)
	}
	chain := strings.TrimPrefix(ds[0].Help, "cycle: ")
	segs := strings.Split(chain, " -> ")
	if len(segs) != 3 {
		t.Fatalf("chain = %q, want three segments", chain)
	}
	marked := 0
	for _, seg := range segs[:2] {
		if strings.Contains(seg, "(via Uses)") {
			marked++
		}
	}
	if marked != 1 || !strings.Contains(segs[0]+segs[1], "provider:select") {
		t.Fatalf("chain = %q, want exactly the nested-Uses edge marked", chain)
	}
}

func TestValidateGraphCycleChainShape(t *testing.T) {
	g := New()
	g.SetDirective("assets")
	g.Node(&Raw{Base: Base{Phase: PhaseWire, Uses: []string{"b"}, Origin: Origin{Pos: vpos("a.go", 3), Directive: "assets"}}, Defines: []string{"a"}, Lines: []string{"a := b"}})
	g.Node(&Assign{Base: Base{Phase: PhaseWire, Origin: Origin{Pos: vpos("a.go", 9), Directive: "web"}}, Var: "b", Expr: "a"})
	ds := g.ValidateGraph()
	if len(ds) != 1 {
		t.Fatalf("diagnostics = %v, want one", ds)
	}
	chain := strings.TrimPrefix(ds[0].Help, "cycle: ")
	segs := strings.Split(chain, " -> ")
	if len(segs) != 3 {
		t.Fatalf("chain = %q, want a closed three-segment chain", chain)
	}
	if !strings.Contains(segs[0], "a.go:3") || !strings.Contains(segs[2], "a.go:3") {
		t.Errorf("chain = %q, want it to close on the starting node", chain)
	}
	if !strings.Contains(segs[0], "(via Uses)") || strings.Contains(segs[1], "(via Uses)") {
		t.Errorf("chain = %q, want Uses attribution only on the declared edge", chain)
	}
}
