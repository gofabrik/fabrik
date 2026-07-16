package main

import (
	"strings"
	"testing"
	"text/template"
)

func renderStarterGoMod(t *testing.T, module, version string) string {
	t.Helper()
	src, err := templatesFS.ReadFile("templates/starter/go.mod.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err := template.New("go.mod").Parse(string(src))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, scaffoldData(module, version)); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestScaffoldGoModPinsFabrik(t *testing.T) {
	got := renderStarterGoMod(t, "hello", "v9.9.9")
	for _, m := range starterFabrikModules {
		if want := m + " v9.9.9"; !strings.Contains(got, want) {
			t.Errorf("go.mod missing %q:\n%s", want, got)
		}
	}
	for _, m := range []string{"config", "assetmapper"} {
		if !strings.Contains(got, "github.com/gofabrik/fabrik/"+m+" v9.9.9") {
			t.Errorf("generator-only module %s not pinned:\n%s", m, got)
		}
	}
	if !strings.HasPrefix(got, "module hello\n") {
		t.Errorf("missing module line:\n%s", got)
	}
}

func TestScaffoldGoModDevelOmitsFabrik(t *testing.T) {
	got := renderStarterGoMod(t, "hello", "")
	if strings.Contains(got, "gofabrik/fabrik") {
		t.Errorf("source build must not pin fabrik:\n%s", got)
	}
	if strings.Contains(got, "require") {
		t.Errorf("source build go.mod should have no require block:\n%s", got)
	}
}

func TestScaffoldDataModeSelection(t *testing.T) {
	if v := scaffoldData("m", "v1.2.3"); v.FabrikVersion != "v1.2.3" || len(v.FabrikModules) != len(starterFabrikModules) {
		t.Fatalf("released: got version=%q modules=%d", v.FabrikVersion, len(v.FabrikModules))
	}
	if v := scaffoldData("m", ""); v.FabrikVersion != "" || v.FabrikModules != nil {
		t.Fatalf("source build should pin nothing, got version=%q modules=%v", v.FabrikVersion, v.FabrikModules)
	}
}
