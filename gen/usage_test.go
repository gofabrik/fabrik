package gen

import (
	"go/token"
	"go/types"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/diag"
)

func usageWorld(t *testing.T) (*Gen, types.Type, types.Type) {
	t.Helper()
	pkg := types.NewPackage("app/x", "x")
	a := types.NewPointer(types.NewNamed(types.NewTypeName(0, pkg, "A", nil), types.NewStruct(nil, nil), nil))
	b := types.NewPointer(types.NewNamed(types.NewTypeName(0, pkg, "B", nil), types.NewStruct(nil, nil), nil))
	g := New()
	g.SetDirective("provider")
	g.BindLazy(a, "", func() (string, diag.Diagnostics) { return "va", nil })
	g.BindLazy(b, "", func() (string, diag.Diagnostics) {
		e, ds, _ := g.Instance(a, "")
		return "vb(" + e + ")", ds
	})
	return g, a, b
}

func TestDemandUsageAcrossFlows(t *testing.T) {
	g, a, b := usageWorld(t)
	s1 := g.AddScope("buildOne", token.Position{}, ScopeRoot{Type: b})
	s2 := g.AddScope("buildTwo", token.Position{}, ScopeRoot{Type: a})
	_ = s1
	_ = s2
	if ds := g.WalkFlows(); ds.HasFatal() {
		t.Fatalf("materialize: %v", ds)
	}
	aKey := demandKey{kind: demandType, key: types.TypeString(a, nil)}
	bKey := demandKey{kind: demandType, key: types.TypeString(b, nil)}
	if got := g.demandFlows(aKey); len(got) != 2 || got[0] != "buildOne" || got[1] != "buildTwo" {
		t.Fatalf("a usage = %v, want both flows sorted", got)
	}
	if got := g.demandFlows(bKey); len(got) != 1 || got[0] != "buildOne" {
		t.Fatalf("b usage = %v", got)
	}
}

func TestDefaultFlowRecordsDemands(t *testing.T) {
	g, a, _ := usageWorld(t)
	if _, ds, ok := g.Instance(a, ""); !ok || ds.HasFatal() {
		t.Fatalf("instance: %v %v", ds, ok)
	}
	aKey := demandKey{kind: demandType, key: types.TypeString(a, nil)}
	if got := g.demandFlows(aKey); len(got) != 1 || got[0] != "default" {
		t.Fatalf("usage = %v, want [default]", got)
	}
}

func TestValidationScopeRecordsNothing(t *testing.T) {
	g, a, _ := usageWorld(t)
	g.AddScope("buildOne", token.Position{}, ScopeRoot{Type: a})
	if ds := g.RunValidationPass(); ds.HasFatal() {
		t.Fatalf("validation: %v", ds)
	}
	aKey := demandKey{kind: demandType, key: types.TypeString(a, nil)}
	if got := g.demandFlows(aKey); len(got) != 0 {
		t.Fatalf("usage after validation only = %v, want none", got)
	}
}

func TestReducedValidationSkipsDemanded(t *testing.T) {
	g := New()
	pkg := types.NewPackage("app/x", "x")
	a := types.NewPointer(types.NewNamed(types.NewTypeName(0, pkg, "A", nil), types.NewStruct(nil, nil), nil))
	c := types.NewPointer(types.NewNamed(types.NewTypeName(0, pkg, "C", nil), types.NewStruct(nil, nil), nil))
	aBuilds, cBuilds := 0, 0
	g.SetDirective("provider")
	g.BindLazy(a, "", func() (string, diag.Diagnostics) { aBuilds++; return "va", nil })
	g.BindLazy(c, "", func() (string, diag.Diagnostics) { cBuilds++; return "vc", nil })
	g.AddScope("buildOne", token.Position{}, ScopeRoot{Type: a})
	if ds := g.WalkFlows(); ds.HasFatal() {
		t.Fatalf("materialize: %v", ds)
	}
	if ds := g.RunValidationPass(); ds.HasFatal() {
		t.Fatalf("validation: %v", ds)
	}
	if aBuilds != 1 {
		t.Fatalf("demanded build ran %d times, want once (skipped by the sweep)", aBuilds)
	}
	if cBuilds != 1 {
		t.Fatalf("undemanded build ran %d times, want once (validation only)", cBuilds)
	}
}

func TestDiagnosedLazyBuildCachesAcrossFlows(t *testing.T) {
	g := New()
	pkg := types.NewPackage("app/x", "x")
	d := types.NewPointer(types.NewNamed(types.NewTypeName(0, pkg, "D", nil), types.NewStruct(nil, nil), nil))
	builds := 0
	g.SetDirective("provider")
	g.BindLazy(d, "", func() (string, diag.Diagnostics) {
		builds++
		var ds diag.Diagnostics
		ds.Error(token.Position{}, "broken", "")
		return "nil", ds
	})
	g.AddScope("buildOne", token.Position{}, ScopeRoot{Type: d})
	g.AddScope("buildTwo", token.Position{}, ScopeRoot{Type: d})
	ds := g.WalkFlows()
	if builds != 1 {
		t.Fatalf("builds = %d, want one (diagnosed build cached across flows)", builds)
	}
	count := 0
	for _, dg := range ds {
		if dg.Message == "broken" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("broken reported %d times, want once", count)
	}
}

func TestCallbackDemandsRecordFlows(t *testing.T) {
	g, a, _ := usageWorld(t)
	ran := 0
	g.ScopePrologue(func() diag.Diagnostics { ran++; return nil })
	g.AddScope("buildOne", token.Position{}, ScopeRoot{Type: a})
	if ds := g.WalkFlows(); ds.HasFatal() {
		t.Fatalf("materialize: %v", ds)
	}
	k := demandKey{kind: demandCallback, key: "prologue", ord: 0}
	if got := g.demandFlows(k); len(got) != 1 || got[0] != "buildOne" {
		t.Fatalf("callback usage = %v, want [buildOne]", got)
	}
}

func TestPathDemandRecordsFlows(t *testing.T) {
	g := New()
	pkg := types.NewPackage("app/x", "x")
	a := types.NewPointer(types.NewNamed(types.NewTypeName(0, pkg, "A", nil), types.NewStruct(nil, nil), nil))
	g.SetDirective("jobs")
	g.BindLazyPath("*jobs.Manager", func() (string, diag.Diagnostics) { return "mgr", nil })
	g.SetDirective("provider")
	g.BindLazy(a, "", func() (string, diag.Diagnostics) {
		e, ds, _ := g.InstancePath("*jobs.Manager")
		return "va(" + e + ")", ds
	})
	g.AddScope("buildOne", token.Position{}, ScopeRoot{Type: a})
	if ds := g.WalkFlows(); ds.HasFatal() {
		t.Fatalf("materialize: %v", ds)
	}
	k := demandKey{kind: demandPath, key: "*jobs.Manager"}
	if got := g.demandFlows(k); len(got) != 1 || got[0] != "buildOne" {
		t.Fatalf("path usage = %v, want [buildOne]", got)
	}
}

func TestEpilogueDemandOrdinal(t *testing.T) {
	g := New()
	pkg := types.NewPackage("app/x", "x")
	a := types.NewPointer(types.NewNamed(types.NewTypeName(0, pkg, "A", nil), types.NewStruct(nil, nil), nil))
	g.SetDirective("provider")
	g.BindLazy(a, "", func() (string, diag.Diagnostics) { return "va", nil })
	g.ScopeEpilogue(func() diag.Diagnostics { return nil })
	g.ScopeEpilogue(func() diag.Diagnostics { return nil })
	g.AddScope("buildOne", token.Position{}, ScopeRoot{Type: a})
	if ds := g.WalkFlows(); ds.HasFatal() {
		t.Fatalf("materialize: %v", ds)
	}
	for ord := 0; ord < 2; ord++ {
		k := demandKey{kind: demandCallback, key: "epilogue", ord: ord}
		if got := g.demandFlows(k); len(got) != 1 || got[0] != "buildOne" {
			t.Fatalf("epilogue %d usage = %v", ord, got)
		}
	}
}

func TestBrokenPathReportsOnceAcrossScopes(t *testing.T) {
	g := New()
	pkg := types.NewPackage("app/x", "x")
	a := types.NewPointer(types.NewNamed(types.NewTypeName(0, pkg, "A", nil), types.NewStruct(nil, nil), nil))
	b := types.NewPointer(types.NewNamed(types.NewTypeName(0, pkg, "B", nil), types.NewStruct(nil, nil), nil))
	builds := 0
	g.SetDirective("jobs")
	g.BindLazyPath("*jobs.Store", func() (string, diag.Diagnostics) {
		builds++
		var ds diag.Diagnostics
		ds.Error(token.Position{}, "store broken", "")
		return "", ds
	})
	g.SetDirective("provider")
	g.BindLazy(a, "", func() (string, diag.Diagnostics) {
		e, ds, _ := g.InstancePath("*jobs.Store")
		return "va(" + e + ")", ds
	})
	g.BindLazy(b, "", func() (string, diag.Diagnostics) {
		e, ds, _ := g.InstancePath("*jobs.Store")
		return "vb(" + e + ")", ds
	})
	g.AddScope("buildOne", token.Position{}, ScopeRoot{Type: a})
	g.AddScope("buildTwo", token.Position{}, ScopeRoot{Type: b})
	ds := g.WalkFlows()
	if builds != 1 {
		t.Fatalf("broken path built %d times, want once across scopes", builds)
	}
	count := 0
	for _, d := range ds {
		if d.Message == "store broken" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("store broken reported %d times, want once", count)
	}
}

func TestPanickingBuildReportsOnceAcrossScopes(t *testing.T) {
	g := New()
	pkg := types.NewPackage("app/x", "x")
	d := types.NewPointer(types.NewNamed(types.NewTypeName(0, pkg, "D", nil), types.NewStruct(nil, nil), nil))
	builds := 0
	g.SetDirective("provider")
	g.BindLazy(d, "", func() (string, diag.Diagnostics) {
		builds++
		panic("boom")
	})
	g.AddScope("buildOne", token.Position{}, ScopeRoot{Type: d})
	g.AddScope("buildTwo", token.Position{}, ScopeRoot{Type: d})
	ds := g.WalkFlows()
	if builds != 1 {
		t.Fatalf("panicking build ran %d times, want once across scopes", builds)
	}
	count := 0
	for _, dg := range ds {
		if strings.Contains(dg.Message, "panicked") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("panic reported %d times, want once", count)
	}
}

func TestEpilogueDiagnosticReportsOnceAcrossFlows(t *testing.T) {
	g := New()
	pkg := types.NewPackage("app/x", "x")
	a := types.NewPointer(types.NewNamed(types.NewTypeName(0, pkg, "A", nil), types.NewStruct(nil, nil), nil))
	g.SetDirective("provider")
	g.BindLazy(a, "", func() (string, diag.Diagnostics) { return "va", nil })
	g.ScopeEpilogue(func() diag.Diagnostics {
		var ds diag.Diagnostics
		ds.Error(token.Position{}, "epilogue broken", "")
		return ds
	})
	g.AddScope("buildOne", token.Position{}, ScopeRoot{Type: a})
	g.AddScope("buildTwo", token.Position{}, ScopeRoot{Type: a})
	all := g.WalkFlows()
	all = append(all, g.RunValidationPass()...)
	count := 0
	for _, d := range all {
		if d.Message == "epilogue broken" {
			count++
		}
	}
	// Identical callback diagnostics collapse across flow and validation replays.
	if count != 1 {
		t.Fatalf("epilogue diagnostic reported %d times, want once", count)
	}
}

func TestBuildRegisteredConflictReportsOnceAcrossScopes(t *testing.T) {
	g := New()
	pkg := types.NewPackage("app/x", "x")
	a := types.NewPointer(types.NewNamed(types.NewTypeName(0, pkg, "A", nil), types.NewStruct(nil, nil), nil))
	inner := types.NewNamed(types.NewTypeName(0, pkg, "T", nil), types.NewStruct(nil, nil), nil)
	g.SetDirective("outer")
	g.BindLazy(a, "", func() (string, diag.Diagnostics) {
		g.Bind(inner, "", "v")
		g.Bind(inner, "", "w")
		return "va", nil
	})
	g.AddScope("buildOne", token.Position{}, ScopeRoot{Type: a})
	g.AddScope("buildTwo", token.Position{}, ScopeRoot{Type: a})
	if ds := g.WalkFlows(); ds.HasFatal() {
		t.Fatalf("materialize: %v", ds)
	}
	first := g.BindConflicts()
	if len(first) != 1 {
		t.Fatalf("conflicts = %v, want the replayed registration reported once", first)
	}
	if ds := g.RunValidationPass(); ds.HasFatal() {
		t.Fatalf("validation: %v", ds)
	}
	if again := g.BindConflicts(); len(again) != 0 {
		t.Fatalf("post-validation conflicts = %v, want none (dedup survives drains)", again)
	}
}

func TestDistinctCallbacksDoNotCollapse(t *testing.T) {
	g := New()
	pkg := types.NewPackage("app/x", "x")
	a := types.NewPointer(types.NewNamed(types.NewTypeName(0, pkg, "A", nil), types.NewStruct(nil, nil), nil))
	g.SetDirective("provider")
	g.BindLazy(a, "", func() (string, diag.Diagnostics) { return "va", nil })
	broken := func() diag.Diagnostics {
		var ds diag.Diagnostics
		ds.Error(token.Position{}, "shared message", "")
		return ds
	}
	g.ScopeEpilogue(broken)
	g.ScopeEpilogue(broken)
	g.AddScope("buildOne", token.Position{}, ScopeRoot{Type: a})
	all := g.WalkFlows()
	count := 0
	for _, d := range all {
		if d.Message == "shared message" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("distinct callbacks reported %d times, want both", count)
	}
}

func TestConflictDedupSurvivesDrains(t *testing.T) {
	g := New()
	pkg := types.NewPackage("app/x", "x")
	a := types.NewNamed(types.NewTypeName(0, pkg, "A", nil), types.NewStruct(nil, nil), nil)
	g.SetDirective("one")
	g.Bind(a, "", "v")
	g.SetDirective("two")
	g.Bind(a, "", "w")
	if ds := g.BindConflicts(); len(ds) != 1 {
		t.Fatalf("first drain = %v, want one", ds)
	}
	g.Bind(a, "", "w")
	if ds := g.BindConflicts(); len(ds) != 0 {
		t.Fatalf("replayed registration after drain = %v, want none", ds)
	}
}
