# Bundle concept - archived

> **Status: ABANDONED, archived on a branch.** This approach is not the
> direction we are taking. It is preserved here (and on its branch) as a
> record of the concept, the code that landed, and the decisions made, so a
> later revisit does not start from zero. Do not build on this without
> revisiting the design first. A different approach supersedes it.

The full design lives in two root-level docs, kept as-is:

- `../BUNDLE.md` - the bundle concept, extension model, and the emitted
  `main.gen.go` shape (~25 review rounds).
- `../TEMPLATE_OVERRIDE.md` - the layered template override design.

This file is the implementation status: what those docs became in code, what
did not get built, and why.

## The concept in one paragraph

A **library** is a standalone Go package. A **bundle** is how you tell the
generator to wire a specific package that does not fit the bare generator's
model - one that contributes across several aspects at once (templates,
assets, migrations, routes) and must be assembled and mounted coherently.
The app writes one annotated provider (`//fabrik:bundle`), the generic
directive looks up the one bundle module that matches its return type, and
the directive owns all emission: force-construct the value, read its
`Manifest()`, fold its contributions into the app's central aggregates, hand
back a merged `Runtime`, and mount whatever its optional `Handler` returns.
Two compiled-in citizens: **directive modules** (compile `//fabrik:*`) and
**bundle modules** (recognize and validate one integration type). Auth was
the first and only intended consumer.

## Build order (from BUNDLE.md)

1. `auth/web` rework - render through an injected `*templates.Set`.
2. Structured boot errors - `templates.SourceError`/`CollisionError`,
   `assetmapper.RootError`, `router.TryHandle`.
3. Contribution aggregator - central per-aspect emitter + provenance sidecar.
4. Reusable provider binder - extracted from `core.Provider`.
5. Generic `//fabrik:bundle` directive + auth bundle module + `auth/bundle`
   value package.

## What was implemented

### Step 1 - auth/web rework (done, tested)

- `auth/web/templates/auth/` - templates restructured into an `auth` section:
  `_layout.html` plus content-only `login.html`, `register.html`,
  `account.html` (page keys `auth/login`, etc.). The old flat documents were
  removed.
- `auth/web/web.go` - renders through a `*templates.Set` instead of a private
  `*template.Template` parse. `//go:embed all:templates`. New `Source()`
  exposes the embedded tree as a `templates.Source` (the bundle base layer).
  `Options.Templates` changed from `*template.Template` to `*templates.Set`
  (the injected, app-merged set); nil builds a standalone set from the embed,
  preserving standalone use.
- `auth/web/go.mod` - added the `templates` dependency.
- `auth/web/web_test.go` - `TestInjectedSetLayerOverridesPage` proves an app
  layer overriding `auth/login.html` wins while the base `register` page is
  preserved (the handoff, end to end).

### Step 2 - structured boot errors (done, tested)

Each carries machine-readable provenance and preserves existing messages;
matchable with `errors.As`.

- `router/router.go` - `TryHandle(pattern, h) error`. Registers like `Handle`
  but recovers ServeMux's conflict panic into an error, so bundle assembly
  never drops into a raw panic on a same-pattern app-vs-bundle mount. Recover
  is safe because `ServeMux.Handle` checks conflicts before mutating.
  `router/router_test.go` - 4 tests (fresh register, conflict-to-error,
  more-specific-allowed, nil-handler still panics).
- `templates/templates.go` - `SourceError{Ref, Err}` (nil-fs, read, parse)
  and `CollisionError{A, B, Err}` (section clash, duplicate page key). Wired
  all four load sites; added `keyOrigin` tracking so the page-key collision
  names both sources. `Error`/`Unwrap` delegate to `Err`, so messages are
  unchanged. `templates/layers_test.go` - provenance assertions on the
  collision and nil-fs cases.
- `assetmapper/` - `RootError{Index, Err}` in `build.go`, wrapping **every**
  root-scoped failure: `normalizeRoots` (assetmapper.go), `discoverImportmap`
  and the stream-hash pass (build.go), and the walk in `collectAssets`
  (compile.go, which gained a `rootIndex` field on `collectedAsset`).
  `assetmapper/build_test.go` - `TestBuildRootErrorProvenance`.

### bundle framework package (done, tested - the reusable core of step 5)

New module `bundle/` (added to `go.work`), path
`github.com/gofabrik/fabrik/bundle`. Runtime-only, no generator or go/types
dependency, so a bundle value package can import it directly.

- `bundle/bundle.go`:
  - `Manifest{Name, Prefix, Namespace, Templates, Assets, Migrations}`,
    `Runtime{Templates, Assets}` (both nilable).
  - `NormalizeManifest` - validates `Name`, defaults+validates `Namespace`,
    returns a copy forcing every migration `Module` to `Name` (fresh slice),
    rejects >1 migration source and a non-matching `Module`.
  - `NormalizePrefix` - canonical subtree pattern; `/auth` and `/auth/` both
    to `/auth/`; `/` valid; empty/malformed error.
  - `Mounts` / `NewMounts` / `Add` - subtree-overlap guard (containment, not
    equality; `/` overlaps everything).
  - `NamedAssetError` / `NamedTemplateError` - `errors.As` the step-2
    structured errors and prefix the owning bundle name via sidecar tables
    (`map[int]string` for assets, `map[[2]int]string` keyed on `(Layer,
    Source)` for templates; a collision names both bundles).
  - `validName` - mirrors assetmapper's `MountAt` rule so a `Name` is valid as
    both asset namespace and migration module.
- `bundle/bundle_test.go` - 10 tests across all of the above.

## Key decisions (folded into BUNDLE.md over the review rounds)

- **Two compiled-in citizens**, no third-party plugin in v1. Bundle modules
  are `Name` + `Match` + `Check` only, with no emission capability; the
  generic directive owns all mechanics.
- **Return-type contract**: a named struct, or a pointer to one, optionally
  with `error`. No interface returns (they defeat exact module matching).
  Provider fallibility is free (`T`/`(T,error)`/`*T`/`(*T,error)`).
- **Method-set validated against `*T`**, so pointer-receiver `Manifest`/
  `Handler` are accepted for both `T` and `*T` returns. Nil guard only on a
  pointer return.
- **Normalized module matching**: `*T` is stripped to the named type before
  `Match`, so a module matches one identity, never both `T` and `*T`.
- **Provider must be exported / package-level / non-generic**. `core.Provider`
  already enforces package-level and non-generic; the export check is the one
  the bundle directive adds (a gap `core.Provider` arguably should close too).
- **`Name` is the single provenance key**: canonical, unique across bundles,
  defaults Namespace and migration Module.
- **Migrations vs auto-create are mutually exclusive**: `SQLiteStore(db,
  SQLiteOptions{Migrations: true})` turns auto-create off and contributes the
  schema through the manifest; both on is a construction error.
- **No raw panic in bundle assembly**: the mount goes through `TryHandle`;
  ServeMux still decides precedence, only the panic becomes a structured
  boot error.
- **Provenance never mutates a library type**: it lives in the aggregator as
  sidecars, matched through the step-2 structured errors.
- **"Bundle is a leaf" was wrong**: contributions are not ordinary DI demand
  edges, so the directive force-constructs exactly once regardless of whether
  anything else depends on the type.

## What was NOT built

- **Step 3 - contribution aggregator.** This is a refactor of the *existing*
  per-directive emission, not a new file: the templates directive emits
  `Load`/`LoadSources` in its own `Emit` (`templates/directive/templates.go`);
  the aggregator must make it emit `LoadLayers` when any bundle contributes a
  layer, do the same for assets and migrations, and thread the provenance
  sidecar. Coordinated surgery across three directive packages and the engine.
- **Step 4 - reusable provider binder.** `core.Provider`'s parsed node, param
  resolution, lazy binding, and built-state are private to package `core`
  (`fabrik/internal/directives/core/provider.go`). A shared binder or a narrow
  force/materialize API must be extracted so `//fabrik:bundle` does not
  recreate provider parsing.
- **Step 5 remainder - the directive, the auth module, the value package.**
  The generic `//fabrik:bundle` directive, the auth bundle module
  (`Name`/`Match`/`Check`), and the `auth/bundle` value package
  (`Options{Store, Session, UI}`, `SQLiteStore`/`WebUI`/`NoUI`,
  `New`/`Manifest`/`Handler`), plus regenerating and validating the demo's
  `main.gen.go`.

### Open design decision that gated `auth/bundle`

To contribute the users-table schema as a `migrations.Source`, the sqlite
store must expose it in migration form, but `auth/store/sqlite` only exposes
`Schema()` as a DDL string. Three options, unresolved:

1. Add an embedded `0001_auth_users.sql` to `auth/store/sqlite` and make it
   the single source of truth, with `Schema()` returning its content (no
   drift). A deliberate library change. (Was the leaning recommendation.)
2. Keep the `Schema()` const and add the `.sql` alongside, with a test
   asserting they match. Two copies.
3. Synthesize an in-memory FS in the bundle from `Schema()`. No library
   change, but pulls `testing/fstest` into production and makes the migration
   synthetic.

This decision, and the direction overall, is what prompted the pause.
