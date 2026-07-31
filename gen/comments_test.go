package gen

import (
	"go/token"
	"strings"
	"testing"
)

func commentGen(level CommentLevel) *Gen {
	g := New()
	g.SetModule("demo")
	g.SetDirective("provider")
	g.SetCommentLevel(level)
	g.SetSourceRoot("/work/app")
	g.Node(&Call{
		Base: Base{Phase: PhaseWire, Origin: Origin{Directive: "provider", Pos: token.Position{Filename: "/work/app/shared/db.go", Line: 14}}},
		Var:  "db", Fn: "shared.NewDB",
	})
	g.Node(&Raw{
		Base:    Base{Phase: PhaseWire, Label: "Jobs", Uses: []string{"db"}, Origin: Origin{Directive: "job"}},
		Lines:   []string{"registry := newRegistry(db)"},
		Defines: []string{"registry"},
	})
	return g
}

func TestCommentLevels(t *testing.T) {
	full, err := commentGen(CommentsFull).Render()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"// Providers", "// provider shared/db.go:14", "// job\n", "// Jobs"} {
		if !strings.Contains(string(full), want) {
			t.Errorf("full output missing %q:\n%s", want, full)
		}
	}

	sections, err := commentGen(CommentsSections).Render()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sections), "// Providers") || !strings.Contains(string(sections), "// Jobs") {
		t.Errorf("sections output missing default comments:\n%s", sections)
	}
	if strings.Contains(string(sections), "shared/db.go:14") {
		t.Errorf("sections output must not carry origin comments:\n%s", sections)
	}

	off, err := commentGen(CommentsOff).Render()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(off), "// Providers") || strings.Contains(string(off), "// Jobs") {
		t.Errorf("off output must carry no generated comments:\n%s", off)
	}
}

func TestZeroValueCommentLevelIsSections(t *testing.T) {
	if CommentLevel(0) != CommentsSections {
		t.Fatal("the zero value must be the current default")
	}
}

func TestScopedEmitterHonorsCommentLevels(t *testing.T) {
	build := func(level CommentLevel) string {
		g := New()
		g.SetModule("demo")
		g.SetDirective("provider")
		g.SetCommentLevel(level)
		g.SetSourceRoot("/work/app")
		g.Node(&Call{
			Base: Base{Phase: PhaseWire, Origin: Origin{Directive: "provider", Pos: token.Position{Filename: "/work/app/shared/db.go", Line: 14}}},
			Var:  "db", Fn: "shared.NewDB",
		})
		src, err := g.Render()
		if err != nil {
			t.Fatal(err)
		}
		return string(src)
	}
	full := build(CommentsFull)
	if !strings.Contains(full, "// provider shared/db.go:14") || !strings.Contains(full, "// Providers") {
		t.Errorf("scoped full output missing comments:\n%s", full)
	}
	off := build(CommentsOff)
	if strings.Contains(off, "// Providers") || strings.Contains(off, "// provider ") {
		t.Errorf("scoped off output must carry no generated comments:\n%s", off)
	}
}

func TestFullCommentsCoverNestedSelectChildren(t *testing.T) {
	g := New()
	g.SetModule("demo")
	g.SetDirective("provider:select")
	g.SetCommentLevel(CommentsFull)
	g.SetSourceRoot("/work/app")
	inner := &Select{
		Base: Base{Phase: PhaseWire, Origin: Origin{Directive: "provider:select", Pos: token.Position{Filename: "/work/app/web/inner.go", Line: 3}}},
		Var:  "inner", Iface: "web.Inner", KeyExpr: `"x"`, FmtPkg: g.Import("fmt"),
		Cases: []Case{{
			Value: "x",
			Result: Call{
				Base: Base{Origin: Origin{Directive: "provider", Pos: token.Position{Filename: "/work/app/web/inner.go", Line: 9}}},
				Var:  "innerX", Fn: "web.NewInnerX", Err: ErrReturn,
			},
		}},
	}
	g.Node(&Select{
		Base: Base{Phase: PhaseWire, Origin: Origin{Directive: "provider:select", Pos: token.Position{Filename: "/work/app/web/outer.go", Line: 3}}},
		Var:  "outer", Iface: "web.Outer", KeyExpr: `"y"`, FmtPkg: g.Import("fmt"),
		Cases: []Case{{
			Value: "y",
			Body:  []Node{inner},
			Result: Call{
				Base: Base{Origin: Origin{Directive: "provider", Pos: token.Position{Filename: "/work/app/web/outer.go", Line: 9}}},
				Var:  "outerY", Fn: "web.NewOuterY", Args: []string{"inner"}, Err: ErrReturn,
			},
		}},
	})
	src, err := g.Render()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "// provider web/inner.go:9") {
		t.Errorf("nested select child provenance missing:\n%s", src)
	}
}

func TestFullCommentsCoverSelectChildren(t *testing.T) {
	g := New()
	g.SetModule("demo")
	g.SetDirective("provider:select")
	g.SetCommentLevel(CommentsFull)
	g.SetSourceRoot("/work/app")
	g.Node(&Select{
		Base: Base{Phase: PhaseWire, Origin: Origin{Directive: "provider:select", Pos: token.Position{Filename: "/work/app/web/greeter.go", Line: 11}}},
		Var:  "greeter", Iface: "web.Greeter", KeyExpr: `"fancy"`, FmtPkg: g.Import("fmt"),
		Cases: []Case{{
			Value: "fancy",
			Body: []Node{&ConfigLoad{
				Base: Base{Phase: PhaseWire, Origin: Origin{Directive: "config", Pos: token.Position{Filename: "/work/app/web/config.go", Line: 5}}},
				Var:  "fancyCfg", Pkg: g.Import("github.com/gofabrik/fabrik/config"), Type: "web.FancyConfig",
			}},
			Result: Call{
				Base: Base{Origin: Origin{Directive: "provider", Pos: token.Position{Filename: "/work/app/web/greeter.go", Line: 29}}},
				Var:  "greeterFancy", Fn: "web.NewFancy", Args: []string{"fancyCfg"}, Err: ErrReturn,
			},
		}},
	})
	src, err := g.Render()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"// provider:select web/greeter.go:11",
		"// config web/config.go:5",
		"// provider web/greeter.go:29",
	} {
		if !strings.Contains(string(src), want) {
			t.Errorf("full output missing %q:\n%s", want, src)
		}
	}
}
