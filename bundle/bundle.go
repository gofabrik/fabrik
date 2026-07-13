// Package bundle holds the runtime types a Fabrik bundle contributes through,
// plus helpers used by generated assembly.
//
// A bundle value (in its own package, e.g. authbundle) exposes:
//
//	Manifest() bundle.Manifest                     // required: contributions, as data
//	Handler(bundle.Runtime) (http.Handler, error)  // optional: routes, rendering through rt
//
// The generator constructs the bundle, reads its Manifest, folds contributions
// into the app aggregates, and hands the merged Runtime back to Handler when
// present.
package bundle

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/gofabrik/fabrik/assetmapper"
	"github.com/gofabrik/fabrik/migrations"
	"github.com/gofabrik/fabrik/templates"
)

// Manifest is a bundle's data contribution. Empty fields contribute nothing.
type Manifest struct {
	// Name is the canonical identity. It defaults the asset Namespace and the
	// migration Module, so it must be a clean relative path token (see
	// NormalizeManifest) and unique across the app's bundles.
	Name string
	// Prefix is the URL mount; "/auth" and "/auth/" both canonicalize to
	// "/auth/". Empty is valid only when nothing is mounted.
	Prefix string
	// Namespace is the asset namespace; empty defaults to Name.
	Namespace string
	// Templates is the bundle's base template layer; the app's trees layer
	// over it and win.
	Templates []templates.Source
	// Assets are namespaced under Namespace by the aggregator.
	Assets []assetmapper.Root
	// Migrations is at most one source; its Module is forced to Name.
	Migrations []migrations.Source
}

// Runtime is the merged assembly the generator hands to Handler after
// aggregation. Both fields are nilable and a Handler must tolerate nil: a
// bundle that contributes no templates or runs where none were built receives
// a nil Set, and likewise for assets.
type Runtime struct {
	Templates *templates.Set
	Assets    *assetmapper.Compiled
}

// NormalizeManifest validates identity, defaults Namespace, and returns a copy
// with every migration Module forced to Name. Prefix is a mount concern and is
// left untouched.
func NormalizeManifest(m Manifest) (Manifest, error) {
	if err := validName(m.Name); err != nil {
		return Manifest{}, fmt.Errorf("bundle Name %w", err)
	}
	ns := m.Namespace
	if ns == "" {
		ns = m.Name
	} else if err := validName(ns); err != nil {
		return Manifest{}, fmt.Errorf("bundle %q: Namespace %w", m.Name, err)
	}
	if len(m.Migrations) > 1 {
		return Manifest{}, fmt.Errorf("bundle %q: at most one migration source, got %d", m.Name, len(m.Migrations))
	}
	migs := make([]migrations.Source, len(m.Migrations))
	for i, s := range m.Migrations {
		if s.Module != "" && s.Module != m.Name {
			return Manifest{}, fmt.Errorf("bundle %q: migration Module %q must be empty or equal Name", m.Name, s.Module)
		}
		s.Module = m.Name
		migs[i] = s
	}
	out := m
	out.Namespace = ns
	out.Migrations = migs
	return out, nil
}

// NormalizePrefix rewrites a mount prefix to a canonical subtree pattern:
// leading slash, trailing slash. "/" is the root subtree.
func NormalizePrefix(p string) (string, error) {
	if p == "" {
		return "", errors.New("prefix is empty")
	}
	if !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("prefix %q must start with /", p)
	}
	trimmed := strings.Trim(p, "/")
	if trimmed == "" {
		return "/", nil // "/" or "//..." collapses to the root subtree
	}
	for _, seg := range strings.Split(trimmed, "/") {
		switch seg {
		case "":
			return "", fmt.Errorf("prefix %q has an empty segment", p)
		case ".", "..":
			return "", fmt.Errorf("prefix %q has a %q segment", p, seg)
		}
	}
	return "/" + trimmed + "/", nil
}

// Mounts guards against overlapping subtree mounts across every bundle and the
// framework's own routes. Add is its one checked operation.
type Mounts struct {
	mounted []mount
}

type mount struct {
	prefix string
	owner  string
}

// NewMounts returns an empty guard.
func NewMounts() *Mounts { return &Mounts{} }

// Add records a canonical subtree prefix for owner.
//
// Overlap is subtree containment, not equality: "/admin/" and
// "/admin/reports/" collide, and "/" collides with everything.
func (m *Mounts) Add(prefix, owner string) error {
	for _, e := range m.mounted {
		if strings.HasPrefix(prefix, e.prefix) || strings.HasPrefix(e.prefix, prefix) {
			return fmt.Errorf("mount %q (%s) overlaps %q (%s)", prefix, owner, e.prefix, e.owner)
		}
	}
	m.mounted = append(m.mounted, mount{prefix: prefix, owner: owner})
	return nil
}

// NamedAssetError prefixes err with the bundle that owns the failing asset
// root, if err carries an [assetmapper.RootError] whose Index is in owners
// (root index -> bundle name). An error from an app root, or any other error,
// is returned unchanged.
func NamedAssetError(err error, owners map[int]string) error {
	var re *assetmapper.RootError
	if errors.As(err, &re) {
		if name, ok := owners[re.Index]; ok {
			return fmt.Errorf("bundle %q: %w", name, err)
		}
	}
	return err
}

// NamedTemplateError prefixes err with the bundle(s) that own the failing
// template source, if err carries a [templates.SourceError] or
// [templates.CollisionError] whose (Layer, Source) is in owners. A collision
// between two bundles names both; one between an app source and a bundle names
// the bundle; an app-only failure is returned unchanged.
func NamedTemplateError(err error, owners map[[2]int]string) error {
	var se *templates.SourceError
	if errors.As(err, &se) {
		if name, ok := owners[[2]int{se.Ref.Layer, se.Ref.Source}]; ok {
			return fmt.Errorf("bundle %q: %w", name, err)
		}
		return err
	}
	var ce *templates.CollisionError
	if errors.As(err, &ce) {
		na, oka := owners[[2]int{ce.A.Layer, ce.A.Source}]
		nb, okb := owners[[2]int{ce.B.Layer, ce.B.Source}]
		switch {
		case oka && okb && na != nb:
			return fmt.Errorf("bundles %q and %q: %w", na, nb, err)
		case oka:
			return fmt.Errorf("bundle %q: %w", na, err)
		case okb:
			return fmt.Errorf("bundle %q: %w", nb, err)
		}
	}
	return err
}

// validName accepts the shared asset namespace and migration module token:
// a non-empty clean relative slash path.
func validName(s string) error {
	if s == "" {
		return errors.New("is empty")
	}
	if strings.HasPrefix(s, "/") || strings.HasSuffix(s, "/") {
		return fmt.Errorf("%q has a leading or trailing slash", s)
	}
	for _, seg := range strings.Split(s, "/") {
		switch seg {
		case "":
			return fmt.Errorf("%q has an empty segment", s)
		case ".", "..":
			return fmt.Errorf("%q has a %q segment", s, seg)
		}
	}
	if path.Clean(s) != s {
		return fmt.Errorf("%q is not a clean path", s)
	}
	return nil
}
