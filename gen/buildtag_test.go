package gen

import (
	"strings"
	"testing"
)

func TestSetBuildTagAddsConstraintBeforePackageClause(t *testing.T) {
	g := New()
	g.SetModule("demo")
	g.SetBuildTag("e2e")
	src, err := g.Render()
	if err != nil {
		t.Fatal(err)
	}
	out := string(src)
	if !strings.HasPrefix(out, "//go:build e2e\n\n") {
		t.Fatalf("output does not start with the build constraint:\n%s", out)
	}
	if i := strings.Index(out, "//go:build e2e"); i >= 0 {
		if p := strings.Index(out, "package main"); p < i {
			t.Fatalf("package clause appears before the build constraint:\n%s", out)
		}
	}
}

func TestNoBuildTagOmitsConstraint(t *testing.T) {
	g := New()
	g.SetModule("demo")
	src, err := g.Render()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "//go:build") {
		t.Fatalf("no buildtag set, but output carries a build constraint:\n%s", src)
	}
}
