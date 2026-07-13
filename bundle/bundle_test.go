package bundle_test

import (
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gofabrik/fabrik/assetmapper"
	"github.com/gofabrik/fabrik/bundle"
	"github.com/gofabrik/fabrik/migrations"
	"github.com/gofabrik/fabrik/templates"
)

func TestNormalizeManifest_DefaultsAndValidates(t *testing.T) {
	m, err := bundle.NormalizeManifest(bundle.Manifest{Name: "auth"})
	if err != nil {
		t.Fatalf("valid manifest: %v", err)
	}
	if m.Namespace != "auth" {
		t.Fatalf("Namespace = %q, want defaulted to Name", m.Namespace)
	}

	explicit, err := bundle.NormalizeManifest(bundle.Manifest{Name: "auth", Namespace: "acct"})
	if err != nil {
		t.Fatalf("explicit namespace: %v", err)
	}
	if explicit.Namespace != "acct" {
		t.Fatalf("Namespace = %q, want the explicit value", explicit.Namespace)
	}
}

func TestNormalizeManifest_RejectsBadNames(t *testing.T) {
	bad := []bundle.Manifest{
		{Name: ""},
		{Name: "/auth"},
		{Name: "auth/"},
		{Name: "auth//x"},
		{Name: "auth/../x"},
		{Name: "."},
	}
	for _, m := range bad {
		if _, err := bundle.NormalizeManifest(m); err == nil {
			t.Fatalf("Name %q: want error", m.Name)
		}
	}
	// A bad explicit Namespace is also rejected, naming the bundle.
	if _, err := bundle.NormalizeManifest(bundle.Manifest{Name: "auth", Namespace: "/bad"}); err == nil {
		t.Fatal("bad Namespace: want error")
	}
}

func TestNormalizeManifest_MigrationModuleForced(t *testing.T) {
	src := migrations.Source{FS: fstest.MapFS{}, Dir: "m"}
	orig := []migrations.Source{src}
	m, err := bundle.NormalizeManifest(bundle.Manifest{Name: "auth", Migrations: orig})
	if err != nil {
		t.Fatalf("empty module: %v", err)
	}
	if m.Migrations[0].Module != "auth" {
		t.Fatalf("Module = %q, want forced to Name", m.Migrations[0].Module)
	}
	// The bundle's own slice is untouched (fresh copy).
	if orig[0].Module != "" {
		t.Fatalf("original slice mutated: Module = %q", orig[0].Module)
	}

	// A non-empty, non-matching Module is rejected.
	if _, err := bundle.NormalizeManifest(bundle.Manifest{
		Name:       "auth",
		Migrations: []migrations.Source{{Module: "users", FS: fstest.MapFS{}}},
	}); err == nil {
		t.Fatal("Module=users: want error")
	}
	// A matching Module is allowed.
	if _, err := bundle.NormalizeManifest(bundle.Manifest{
		Name:       "auth",
		Migrations: []migrations.Source{{Module: "auth", FS: fstest.MapFS{}}},
	}); err != nil {
		t.Fatalf("Module=Name: %v", err)
	}
}

func TestNormalizeManifest_RejectsMultipleMigrations(t *testing.T) {
	_, err := bundle.NormalizeManifest(bundle.Manifest{
		Name: "auth",
		Migrations: []migrations.Source{
			{FS: fstest.MapFS{}, Dir: "a"},
			{FS: fstest.MapFS{}, Dir: "b"},
		},
	})
	if err == nil {
		t.Fatal("two migration sources: want error")
	}
}

func TestNormalizePrefix(t *testing.T) {
	ok := map[string]string{
		"/auth":     "/auth/",
		"/auth/":    "/auth/",
		"/a/b":      "/a/b/",
		"/":         "/",
		"//":        "/",
		"/admin/x/": "/admin/x/",
	}
	for in, want := range ok {
		got, err := bundle.NormalizePrefix(in)
		if err != nil {
			t.Fatalf("NormalizePrefix(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("NormalizePrefix(%q) = %q, want %q", in, got, want)
		}
	}
	bad := []string{"", "auth", "auth/", "/auth//x", "/auth/../x", "/auth/./x"}
	for _, in := range bad {
		if _, err := bundle.NormalizePrefix(in); err == nil {
			t.Fatalf("NormalizePrefix(%q): want error", in)
		}
	}
	// Idempotent on a canonical value.
	if got, _ := bundle.NormalizePrefix("/auth/"); got != "/auth/" {
		t.Fatalf("idempotency: %q", got)
	}
}

func TestMountsOverlap(t *testing.T) {
	m := bundle.NewMounts()
	if err := m.Add("/assets/", "assets route"); err != nil {
		t.Fatalf("first add: %v", err)
	}
	// A sibling is fine.
	if err := m.Add("/auth/", "auth"); err != nil {
		t.Fatalf("sibling add: %v", err)
	}
	// A subtree of an existing mount overlaps.
	if err := m.Add("/auth/login/", "other"); err == nil {
		t.Fatal("subtree of /auth/: want overlap error")
	}
	// A parent of an existing mount overlaps too.
	if err := m.Add("/assets/x/", "x"); err == nil {
		t.Fatal("child of /assets/: want overlap error")
	}
	// Exact duplicate overlaps.
	if err := m.Add("/auth/", "dup"); err == nil {
		t.Fatal("exact dup: want overlap error")
	}
}

func TestMountsRootOverlapsEverything(t *testing.T) {
	m := bundle.NewMounts()
	if err := m.Add("/assets/", "assets"); err != nil {
		t.Fatalf("add assets: %v", err)
	}
	if err := m.Add("/", "root bundle"); err == nil {
		t.Fatal(`"/" over an existing mount: want overlap error`)
	}
}

func TestNamedAssetError(t *testing.T) {
	owners := map[int]string{1: "auth"}
	// A RootError at a bundle-owned index is prefixed with the name.
	err := &assetmapper.RootError{Index: 1, Err: fmt.Errorf("walk: boom")}
	named := bundle.NamedAssetError(err, owners)
	if !strings.Contains(named.Error(), `bundle "auth"`) {
		t.Fatalf("named = %q, want the bundle name", named.Error())
	}
	// An app-owned index (not in owners) is unchanged.
	appErr := &assetmapper.RootError{Index: 0, Err: fmt.Errorf("walk: boom")}
	if got := bundle.NamedAssetError(appErr, owners); strings.Contains(got.Error(), "bundle") {
		t.Fatalf("app error should be unnamed: %q", got.Error())
	}
	// A non-RootError is unchanged.
	plain := fmt.Errorf("unrelated")
	if got := bundle.NamedAssetError(plain, owners); got != plain {
		t.Fatalf("plain error changed: %v", got)
	}
}

func TestNamedTemplateError(t *testing.T) {
	owners := map[[2]int]string{
		{0, 0}: "auth",
		{0, 1}: "billing",
	}
	// SourceError at a bundle source.
	se := &templates.SourceError{Ref: templates.Ref{Layer: 0, Source: 0, Dir: "t"}, Err: fmt.Errorf("parse fail")}
	if got := bundle.NamedTemplateError(se, owners); !strings.Contains(got.Error(), `bundle "auth"`) {
		t.Fatalf("source error = %q, want auth", got.Error())
	}
	// CollisionError between two bundles names both.
	ce := &templates.CollisionError{
		A:   templates.Ref{Layer: 0, Source: 0},
		B:   templates.Ref{Layer: 0, Source: 1},
		Err: fmt.Errorf("section clash"),
	}
	got := bundle.NamedTemplateError(ce, owners)
	if !strings.Contains(got.Error(), `"auth"`) || !strings.Contains(got.Error(), `"billing"`) {
		t.Fatalf("collision = %q, want both bundles", got.Error())
	}
	// An app-layer source (not in owners) is unchanged.
	appSE := &templates.SourceError{Ref: templates.Ref{Layer: 1, Source: 0}, Err: fmt.Errorf("x")}
	if out := bundle.NamedTemplateError(appSE, owners); strings.Contains(out.Error(), "bundle") {
		t.Fatalf("app source should be unnamed: %q", out.Error())
	}
}
