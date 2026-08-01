package gen

import (
	"fmt"
	"go/token"
	"go/types"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/diag"
)

func TestGlobalEagerBindConflict(t *testing.T) {
	g := New()
	g.SetDirective("first:eager")
	g.Bind(types.Typ[types.Int], "", "first")
	g.SetDirective("second:eager")
	g.Bind(types.Typ[types.Int], "", "second")

	assertBindConflict(t, g.BindConflicts(), "first:eager", "second:eager")
	if ds := g.BindConflicts(); len(ds) != 0 {
		t.Fatalf("second BindConflicts() = %v, want drained diagnostics", ds)
	}
	expr, ds, ok := g.Instance(types.Typ[types.Int], "")
	if !ok || len(ds) != 0 || expr != "first" {
		t.Fatalf("Instance() = %q, %v, %v, want first binding", expr, ds, ok)
	}
}

func TestScopedEagerBindConflict(t *testing.T) {
	g := New()
	g.enterScope(&Scope{}, false)
	defer func() { g.scope = nil }()

	g.SetDirective("first:scoped")
	g.Bind(types.Typ[types.Int], "", "first")
	g.SetDirective("second:scoped")
	g.Bind(types.Typ[types.Int], "", "second")

	assertBindConflict(t, g.BindConflicts(), "first:scoped", "second:scoped")
	expr, ds, ok := g.Instance(types.Typ[types.Int], "")
	if !ok || len(ds) != 0 || expr != "first" {
		t.Fatalf("Instance() = %q, %v, %v, want first binding", expr, ds, ok)
	}
}

func TestLazyBindConflict(t *testing.T) {
	g := New()
	g.SetDirective("first:lazy")
	g.BindLazy(types.Typ[types.Int], "", func() (string, diag.Diagnostics) {
		return "first", nil
	})
	g.SetDirective("second:lazy")
	g.BindLazy(types.Typ[types.Int], "", func() (string, diag.Diagnostics) {
		return "second", nil
	})

	assertBindConflict(t, g.BindConflicts(), "first:lazy", "second:lazy")
	expr, ds, ok := g.Instance(types.Typ[types.Int], "")
	if !ok || len(ds) != 0 || expr != "first" {
		t.Fatalf("Instance() = %q, %v, %v, want first binding", expr, ds, ok)
	}
}

func TestLazyPathBindConflict(t *testing.T) {
	g := New()
	g.SetDirective("first:path")
	g.BindLazyPath("example.com/app.Value", func() (string, diag.Diagnostics) {
		return "first", nil
	})
	g.SetDirective("second:path")
	g.BindLazyPath("example.com/app.Value", func() (string, diag.Diagnostics) {
		return "second", nil
	})

	assertBindConflict(t, g.BindConflicts(), "first:path", "second:path")
	expr, ds, ok := g.InstancePath("example.com/app.Value")
	if !ok || len(ds) != 0 || expr != "first" {
		t.Fatalf("InstancePath() = %q, %v, %v, want first binding", expr, ds, ok)
	}
}

func TestDistinctBindKeysDoNotConflict(t *testing.T) {
	g := New()
	g.SetDirective("eager")
	g.Bind(types.Typ[types.Int], "", "defaultInt")
	g.Bind(types.Typ[types.Int], "named", "namedInt")
	g.Bind(types.Typ[types.String], "", "defaultString")
	g.SetDirective("lazy")
	g.BindLazy(types.Typ[types.Bool], "", func() (string, diag.Diagnostics) {
		return "defaultBool", nil
	})
	g.BindLazy(types.Typ[types.Bool], "named", func() (string, diag.Diagnostics) {
		return "namedBool", nil
	})
	g.BindLazyPath("example.com/app.One", func() (string, diag.Diagnostics) {
		return "one", nil
	})
	g.BindLazyPath("example.com/app.Two", func() (string, diag.Diagnostics) {
		return "two", nil
	})

	if ds := g.BindConflicts(); len(ds) != 0 {
		t.Fatalf("BindConflicts() = %v, want none", ds)
	}
}

func TestBindingRegistrationAfterRenderPanics(t *testing.T) {
	g := New()
	g.SetModule("example.com/app")
	if _, err := g.Render(); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		register func()
	}{
		{
			name: "eager",
			register: func() {
				g.Bind(types.Typ[types.Int], "", "value")
			},
		},
		{
			name: "lazy",
			register: func() {
				g.BindLazy(types.Typ[types.Int], "", func() (string, diag.Diagnostics) {
					return "value", nil
				})
			},
		},
		{
			name: "lazy path",
			register: func() {
				g.BindLazyPath("example.com/app.Value", func() (string, diag.Diagnostics) {
					return "value", nil
				})
			},
		},
		{
			name: "published path",
			register: func() {
				g.BindPath("example.com/app.Value", "value")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			defer func() {
				p := recover()
				if p == nil || !strings.Contains(fmt.Sprint(p), "after Render") {
					t.Fatalf("panic = %v, want post-Render registration error", p)
				}
			}()
			test.register()
		})
	}
}

func assertBindConflict(t *testing.T, ds diag.Diagnostics, owners ...string) {
	t.Helper()
	if len(ds) != 1 || ds[0].Severity != diag.SevError {
		t.Fatalf("BindConflicts() = %v, want one error", ds)
	}
	if ds[0].Pos.IsValid() {
		t.Errorf("diagnostic position = %s, want unpositioned", ds[0].Pos)
	}
	for _, owner := range owners {
		if !strings.Contains(ds[0].Message, owner) {
			t.Errorf("diagnostic %q missing owner %q", ds[0].Message, owner)
		}
	}
}

func TestPositionedLazyConflictNamesFirstSite(t *testing.T) {
	g := New()
	pkg := types.NewPackage("app/x", "x")
	obj := types.NewTypeName(0, pkg, "T", nil)
	named := types.NewNamed(obj, types.NewStruct(nil, nil), nil)

	g.SetDirective("first")
	g.BindLazyAt(named, "", token.Position{Filename: "a.go", Line: 3, Column: 1},
		func() (string, diag.Diagnostics) { return "a", nil })
	g.SetDirective("second")
	g.BindLazyAt(named, "", token.Position{Filename: "b.go", Line: 9, Column: 1},
		func() (string, diag.Diagnostics) { return "b", nil })

	ds := g.BindConflicts()
	if len(ds) != 1 {
		t.Fatalf("conflicts = %v, want one", ds)
	}
	if ds[0].Pos.Filename != "b.go" {
		t.Errorf("position = %v, want the second registration's", ds[0].Pos)
	}
	if !strings.Contains(ds[0].Help, "a.go:3") {
		t.Errorf("help = %q, want the first declaration's position", ds[0].Help)
	}
}

func TestConflictFromValidationPassIsRetrievable(t *testing.T) {
	g := New()
	pkg := types.NewPackage("app/x", "x")
	obj := types.NewTypeName(0, pkg, "T", nil)
	named := types.NewNamed(obj, types.NewStruct(nil, nil), nil)
	ptr := types.NewPointer(named)

	g.SetDirective("outer")
	g.BindLazy(ptr, "", func() (string, diag.Diagnostics) {
		g.Bind(named, "", "v")
		g.Bind(named, "", "w")
		return "v", nil
	})
	g.AddScope("buildX", token.Position{}, ScopeRoot{Type: ptr})
	if ds := g.RunValidationPass(); ds.HasFatal() {
		t.Fatalf("validation: %v", ds)
	}
	ds := g.BindConflicts()
	if len(ds) != 1 {
		t.Fatalf("conflicts after validation = %v, want one", ds)
	}
}
