package templates_test

import (
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gofabrik/fabrik/templates"
)

func render(t *testing.T, set *templates.Set, name string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	if err := set.Render(rec, name, nil); err != nil {
		t.Fatalf("Render(%s): %v", name, err)
	}
	return rec.Body.String()
}

func TestLoadLayers_PageOverride(t *testing.T) {
	base := fstest.MapFS{
		"t/auth/_layout.html":  &fstest.MapFile{Data: []byte(`L[{{ block "content" . }}{{ end }}]`)},
		"t/auth/login.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}BASE-LOGIN{{ end }}`)},
		"t/auth/register.html": &fstest.MapFile{Data: []byte(`{{ define "content" }}BASE-REGISTER{{ end }}`)},
	}
	app := fstest.MapFS{
		"t/auth/login.html": &fstest.MapFile{Data: []byte(`{{ define "content" }}APP-LOGIN{{ end }}`)},
	}
	set, err := templates.LoadLayers([][]templates.Source{
		{{FS: base, Dir: "t"}},
		{{FS: app, Dir: "t"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := render(t, set, "auth/login"); got != "L[APP-LOGIN]" {
		t.Fatalf("auth/login = %q, want overlay page to win", got)
	}
	if got := render(t, set, "auth/register"); got != "L[BASE-REGISTER]" {
		t.Fatalf("auth/register = %q, want base page to survive", got)
	}
}

func TestLoadLayers_LayoutOverride(t *testing.T) {
	base := fstest.MapFS{
		"t/auth/_layout.html": &fstest.MapFile{Data: []byte(`BASE[{{ block "content" . }}{{ end }}]`)},
		"t/auth/home.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}H{{ end }}`)},
	}
	app := fstest.MapFS{
		"t/auth/_layout.html": &fstest.MapFile{Data: []byte(`APP[{{ block "content" . }}{{ end }}]`)},
	}
	set, err := templates.LoadLayers([][]templates.Source{
		{{FS: base, Dir: "t"}},
		{{FS: app, Dir: "t"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := render(t, set, "auth/home"); got != "APP[H]" {
		t.Fatalf("auth/home = %q, want overlay layout to win", got)
	}
}

func TestLoadLayers_PartialOverride_SameName(t *testing.T) {
	base := fstest.MapFS{
		"t/auth/_layout.html": &fstest.MapFile{Data: []byte(`{{ template "card" . }}|{{ block "content" . }}{{ end }}`)},
		"t/auth/_card.html":   &fstest.MapFile{Data: []byte(`{{ define "card" }}BASE-CARD{{ end }}`)},
		"t/auth/home.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}H{{ end }}`)},
	}
	app := fstest.MapFS{
		"t/auth/_card.html": &fstest.MapFile{Data: []byte(`{{ define "card" }}APP-CARD{{ end }}`)},
	}
	set, err := templates.LoadLayers([][]templates.Source{
		{{FS: base, Dir: "t"}},
		{{FS: app, Dir: "t"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := render(t, set, "auth/home"); got != "APP-CARD|H" {
		t.Fatalf("auth/home = %q, want overlay partial to win", got)
	}
}

// Later layers can override template definitions without sharing filenames.
func TestLoadLayers_PartialOverride_AcrossFilenames(t *testing.T) {
	base := fstest.MapFS{
		"t/auth/_layout.html": &fstest.MapFile{Data: []byte(`{{ template "card" . }}|{{ block "content" . }}{{ end }}`)},
		"t/auth/_card.html":   &fstest.MapFile{Data: []byte(`{{ define "card" }}BASE-CARD{{ end }}`)},
		"t/auth/home.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}H{{ end }}`)},
	}
	app := fstest.MapFS{
		"t/auth/_widget.html": &fstest.MapFile{Data: []byte(`{{ define "card" }}APP-CARD{{ end }}`)},
	}
	set, err := templates.LoadLayers([][]templates.Source{
		{{FS: base, Dir: "t"}},
		{{FS: app, Dir: "t"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := render(t, set, "auth/home"); got != "APP-CARD|H" {
		t.Fatalf("auth/home = %q, want later-layer define to win by parse order", got)
	}
}

func TestLoadLayers_WithinLayerCollision(t *testing.T) {
	a := fstest.MapFS{"t/web/x.html": &fstest.MapFile{Data: []byte(`x`)}}
	b := fstest.MapFS{"t/web/y.html": &fstest.MapFile{Data: []byte(`y`)}}
	other := fstest.MapFS{"t/_default/_layout.html": &fstest.MapFile{Data: []byte(`{{ block "content" . }}{{ end }}`)}}
	_, err := templates.LoadLayers([][]templates.Source{
		{{FS: a, Dir: "t"}, {FS: b, Dir: "t"}},
		{{FS: other, Dir: "t"}},
	})
	if err == nil || !strings.Contains(err.Error(), `section "web" comes from layer 0 source 0`) {
		t.Fatalf("err = %v, want within-layer collision naming the layer", err)
	}
}

func TestLoadLayers_EmptyLayersIgnored(t *testing.T) {
	real := fstest.MapFS{
		"t/_default/_layout.html": &fstest.MapFile{Data: []byte(`[{{ block "content" . }}{{ end }}]`)},
		"t/_default/home.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}ok{{ end }}`)},
	}
	set, err := templates.LoadLayers([][]templates.Source{
		nil,
		{{FS: real, Dir: "t"}},
		{},
	})
	if err != nil {
		t.Fatalf("empty layers should be ignored: %v", err)
	}
	if got := render(t, set, "home"); got != "[ok]" {
		t.Fatalf("home = %q", got)
	}
}

func TestLoadLayers_NilSourceFS(t *testing.T) {
	real := fstest.MapFS{
		"t/_default/_layout.html": &fstest.MapFile{Data: []byte(`x`)},
		"t/_default/home.html":    &fstest.MapFile{Data: []byte(`x`)},
	}
	_, err := templates.LoadLayers([][]templates.Source{
		{{FS: real, Dir: "t"}},
		{{FS: nil, Dir: "t"}},
	})
	if err == nil || !strings.Contains(err.Error(), "nil filesystem") {
		t.Fatalf("err = %v, want nil-filesystem error", err)
	}
}

// Layering applies to _default before cross-section fallback.
func TestLoadLayers_DefaultLayering(t *testing.T) {
	bundle := fstest.MapFS{
		"t/_default/_layout.html": &fstest.MapFile{Data: []byte(`BASE-CHROME[{{ template "_nav" . }}|{{ block "content" . }}{{ end }}]`)},
		"t/_default/_nav.html":    &fstest.MapFile{Data: []byte(`{{ define "_nav" }}BASE-NAV{{ end }}`)},
		"t/auth/login.html":       &fstest.MapFile{Data: []byte(`{{ define "content" }}LOGIN{{ end }}`)},
	}
	app := fstest.MapFS{
		"t/_default/_layout.html": &fstest.MapFile{Data: []byte(`APP-CHROME[{{ template "_nav" . }}|{{ block "content" . }}{{ end }}]`)},
		"t/_default/_nav.html":    &fstest.MapFile{Data: []byte(`{{ define "_nav" }}APP-NAV{{ end }}`)},
	}
	set, err := templates.LoadLayers([][]templates.Source{
		{{FS: bundle, Dir: "t"}},
		{{FS: app, Dir: "t"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := render(t, set, "auth/login"); got != "APP-CHROME[APP-NAV|LOGIN]" {
		t.Fatalf("auth/login = %q, want app _default layout and nav to reskin the bundle page", got)
	}
}

func TestLoadLayers_Overrides(t *testing.T) {
	base := fstest.MapFS{
		"t/auth/_layout.html": &fstest.MapFile{Data: []byte(`{{ template "card" . }}|{{ block "content" . }}{{ end }}`)},
		"t/auth/_card.html":   &fstest.MapFile{Data: []byte(`{{ define "card" }}BASE{{ end }}`)},
		"t/auth/login.html":   &fstest.MapFile{Data: []byte(`{{ define "content" }}BASE{{ end }}`)},
	}
	app := fstest.MapFS{
		"t/auth/_layout.html": &fstest.MapFile{Data: []byte(`{{ template "card" . }}|{{ block "content" . }}{{ end }}`)},
		"t/auth/_card.html":   &fstest.MapFile{Data: []byte(`{{ define "card" }}APP{{ end }}`)},
		"t/auth/login.html":   &fstest.MapFile{Data: []byte(`{{ define "content" }}APP{{ end }}`)},
	}
	set, err := templates.LoadLayers([][]templates.Source{
		{{FS: base, Dir: "t"}},
		{{FS: app, Dir: "t"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ovr := set.Overrides()
	want := []templates.Override{
		{Section: "auth", Name: "_layout.html", Kind: templates.KindLayout, Winner: templates.Ref{Layer: 1, Source: 0, Dir: "t"}, Loser: templates.Ref{Layer: 0, Source: 0, Dir: "t"}},
		{Section: "auth", Name: "login.html", Kind: templates.KindPage, Winner: templates.Ref{Layer: 1, Source: 0, Dir: "t"}, Loser: templates.Ref{Layer: 0, Source: 0, Dir: "t"}},
		{Section: "auth", Name: "_card.html", Kind: templates.KindPartial, Winner: templates.Ref{Layer: 1, Source: 0, Dir: "t"}, Loser: templates.Ref{Layer: 0, Source: 0, Dir: "t"}},
	}
	if len(ovr) != len(want) {
		t.Fatalf("Overrides() = %+v, want %d entries", ovr, len(want))
	}
	for i := range want {
		if ovr[i] != want[i] {
			t.Fatalf("Overrides()[%d] = %+v, want %+v", i, ovr[i], want[i])
		}
	}
	ovr[0].Section = "mutated"
	if again := set.Overrides(); again[0].Section != "auth" {
		t.Fatalf("Overrides() returned a shared slice; got %q after caller mutation", again[0].Section)
	}
}

// Multi-layer shadows are recorded pairwise in layer order.
func TestLoadLayers_ThreeLayers(t *testing.T) {
	base := fstest.MapFS{
		"t/auth/_layout.html": &fstest.MapFile{Data: []byte(`{{ template "card" . }}|{{ block "content" . }}{{ end }}`)},
		"t/auth/_card.html":   &fstest.MapFile{Data: []byte(`{{ define "card" }}BASE{{ end }}`)},
		"t/auth/login.html":   &fstest.MapFile{Data: []byte(`{{ define "content" }}BASE{{ end }}`)},
	}
	theme := fstest.MapFS{
		"t/auth/_card.html": &fstest.MapFile{Data: []byte(`{{ define "card" }}THEME{{ end }}`)},
		"t/auth/login.html": &fstest.MapFile{Data: []byte(`{{ define "content" }}THEME{{ end }}`)},
	}
	app := fstest.MapFS{
		"t/auth/_card.html": &fstest.MapFile{Data: []byte(`{{ define "card" }}APP{{ end }}`)},
		"t/auth/login.html": &fstest.MapFile{Data: []byte(`{{ define "content" }}APP{{ end }}`)},
	}
	set, err := templates.LoadLayers([][]templates.Source{
		{{FS: base, Dir: "t"}},
		{{FS: theme, Dir: "t"}},
		{{FS: app, Dir: "t"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := render(t, set, "auth/login"); got != "APP|APP" {
		t.Fatalf("auth/login = %q, want the last layer to win both", got)
	}
	l0, l1, l2 := templates.Ref{Layer: 0, Source: 0, Dir: "t"}, templates.Ref{Layer: 1, Source: 0, Dir: "t"}, templates.Ref{Layer: 2, Source: 0, Dir: "t"}
	want := []templates.Override{
		{Section: "auth", Name: "login.html", Kind: templates.KindPage, Winner: l1, Loser: l0},
		{Section: "auth", Name: "login.html", Kind: templates.KindPage, Winner: l2, Loser: l1},
		{Section: "auth", Name: "_card.html", Kind: templates.KindPartial, Winner: l1, Loser: l0},
		{Section: "auth", Name: "_card.html", Kind: templates.KindPartial, Winner: l2, Loser: l1},
	}
	ovr := set.Overrides()
	if len(ovr) != len(want) {
		t.Fatalf("Overrides() = %+v, want %d entries", ovr, len(want))
	}
	for i := range want {
		if ovr[i] != want[i] {
			t.Fatalf("Overrides()[%d] = %+v, want %+v", i, ovr[i], want[i])
		}
	}
}

func TestLoadLayers_SingleLayerEqualsLoadSources(t *testing.T) {
	shared := fstest.MapFS{
		"t/_default/_layout.html": &fstest.MapFile{Data: []byte(`layout[{{ block "content" . }}{{ end }}]`)},
		"t/_default/_nav.html":    &fstest.MapFile{Data: []byte(`nav`)},
	}
	web := fstest.MapFS{
		"t/web/home.html": &fstest.MapFile{Data: []byte(`{{ define "content" }}home{{ end }}`)},
	}
	srcs := []templates.Source{{FS: shared, Dir: "t"}, {FS: web, Dir: "t"}}

	a, err := templates.LoadSources(srcs)
	if err != nil {
		t.Fatalf("LoadSources: %v", err)
	}
	b, err := templates.LoadLayers([][]templates.Source{srcs})
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	if strings.Join(a.Names(), ",") != strings.Join(b.Names(), ",") {
		t.Fatalf("Names differ: %v vs %v", a.Names(), b.Names())
	}
	if render(t, a, "web/home") != render(t, b, "web/home") {
		t.Fatal("renders differ between LoadSources and single-layer LoadLayers")
	}
	if len(a.Overrides()) != 0 || len(b.Overrides()) != 0 {
		t.Fatalf("single layer should record no overrides: %v, %v", a.Overrides(), b.Overrides())
	}
}
