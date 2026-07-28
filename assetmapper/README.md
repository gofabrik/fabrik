# assetmapper

Frontend assets for Go without Node or a bundler: hash-addressed URLs
for immutable caching, ES modules with importmaps, JS / CSS reference
rewriting, and vendoring from jspm.io. Stdlib only.

## The model

Assets are plain files in a directory - CSS, ES modules, images,
fonts. The library maps each logical path (`app.css`) to a
hash-addressed public URL (`/assets/app-4c9d02ef7129e84f21d3.css`), rewriting the
references inside JS and CSS (`import "./nav.js"`, `url("bg.png")`,
`@import`) so the whole graph is hash-addressed and cacheable forever.
JavaScript stays standard ES modules the browser runs directly; bare
specifiers like `import htmx from "htmx"` resolve through an
importmap rendered into the page.

Circular module and stylesheet graphs are supported. Every member of a
strongly connected component receives one shared graph digest, so changing any
member invalidates the complete cycle and every importer downstream from it.

## Usage

Embed the sources, compile in memory at startup:

```go
//go:embed all:assets
var assets embed.FS

compiled, err := assetmapper.Build([]assetmapper.Root{
	{FS: assets, Dir: "assets"},
}, nil)
if err != nil {
	log.Fatal(err)
}

mux.Handle("/assets/", compiled.Handler())

tmpl := template.Must(template.New("page").
	Funcs(compiled.FuncMap()).
	ParseFS(pages, "page.html"))
```

In templates:

```html
<link rel="stylesheet" href="{{ asset "app.css" }}">
{{ importmap "app" }}
```

Use the `all:` embed prefix. Plain `//go:embed assets` silently drops
`_`-prefixed files (`_variables.css`) and dot-prefixed files
(`.well-known/`), and the first symptom is a 404 in production.

`Build` hashes every file, rewrites JS / CSS references to the hashed
URLs (transitively - a changed image re-busts the CSS referencing it,
which re-busts anything importing that CSS), and validates the
importmap. It is deterministic from content, so URLs are stable
across replicas. `Build` snapshots every served byte in memory, so even a
mutable source filesystem cannot change or remove content after its hashed
URL, ETag, and length have been fixed.

`Handler` owns its prefix stripping - no `http.StripPrefix`. It
serves GET and HEAD (405 otherwise), answers `If-None-Match` with
304, and sets `Cache-Control: public, max-age=31536000, immutable`,
which is safe because every served name is derived from its final content or,
for a cycle, from the complete component graph. Strong ETags still identify
each member's exact served bytes.

`Check(roots, im, opts...)` runs the same pipeline and reports the
error `Build` would, without keeping the result - wire it into CI so
a broken reference or importmap fails the build, not the deploy.
Relative JS imports/exports and CSS imports must resolve to an asset, and bare
JS specifiers must exist in the importmap. Root-relative references and CSS
`url()` targets may intentionally be application endpoints, so they remain
external by default; pass `WithStrictAssetURLs()` to require those targets to
resolve as assets too.

## Roots

Multiple packages contribute trees to one logical namespace; roots
are searched in order and the first match wins, so a listed-first
root deliberately shadows later ones. `Dir` selects the subdirectory
inside the FS (an `embed.FS` carries its `assets/` prefix); `MountAt`
prefixes the root's files with a namespace segment:

```go
assetmapper.Build([]assetmapper.Root{
	{FS: shared.Assets, Dir: "assets"},
	{FS: web.Assets, Dir: "assets", MountAt: "web"},
}, nil)
```

## Importmap and vendoring

`importmap.json` lives at the top of an asset tree, so it embeds and
travels with the sources it describes. A nil importmap argument to
`Build` discovers it there (two roots carrying one is an error;
none means empty). Entries map bare specifiers to local assets
(`"path"`) or vendored packages (`"version"`); entries marked
`"entrypoint": true` can be rendered into the page:

```json
{
  "app": {"path": "app.js", "entrypoint": true},
  "htmx": {"version": "2.0.3"}
}
```

`{{ importmap "app" }}` emits the `<script type="importmap">` block,
`<link rel="modulepreload">` tags for the transitive module graph,
and the entrypoint tag. JS entrypoints load as external module
scripts, so the importmap is the page's only inline script. CSP
variants (`importmap_nonce`, `module_preload_links_nonce`) take a
nonce for `script-src 'nonce-...'` policies; for hash-based policies,
`Compiled.CSPImportmapHash()` returns the ready
`'sha256-...'` source for that one inline script, fixed at build.

## Adding a third-party package

`cmd/assetmapper` is the vendoring CLI - it downloads a package and
its transitive dependencies from jspm.io into `<dir>/vendor/` and
registers them in `<dir>/importmap.json`:

```sh
go install github.com/gofabrik/fabrik/assetmapper/cmd/assetmapper@latest

assetmapper require -dir web/assets htmx.org@2.0.3
assetmapper remove  -dir web/assets htmx.org
assetmapper prune   -dir web/assets
```

Then in any module:

```js
import htmx from "htmx.org";
```

Vendored files are immutable, content-addressed assets afterwards - committed,
embedded, hashed again by the production compiler, and importmap-resolved.
The importmap switches to a new artifact only after its bytes are durable.
`vendor.lock.json` records each artifact's
exact version, type, final source URL, downloaded size and SHA-256, plus the
published size and SHA-256 so committed files can be verified with
`VendorLock.Verify`. There is no install step on other machines: the files and
provenance lock live in the repository.

The lock also distinguishes direct requirements from the resolved closure and
records dependency edges and direct owners. Every `require` or `remove`
re-resolves all direct pins together and atomically replaces the complete
lock/importmap graph. This prevents a later install from silently changing a
shared transitive dependency without checking its other owners. Only direct
requirements can be removed; orphaned transitives disappear from the active
graph automatically.

The CLI commits the lock and importmap through a recovery journal. If a process
is interrupted between those two atomic metadata writes, the next vendoring
command completes the recorded commit before doing new work. Removed versions
remain as harmless unreferenced bytes until `assetmapper prune`.

JSPM resolution requires HTTPS and same-host redirects by default, blocks
private-network destinations, and limits package count, individual package
size, total resolution size, and generator response size. Trusted development
mirrors can opt into HTTP, private-network access, or cross-host redirects on
`JSPMResolver`.

The same flow works without the CLI: use the `Vendor` type with a
`PackageResolver` (jspm.io shipped, others pluggable). Keep vendored files,
`importmap.json`, and `vendor.lock.json` together; creating versioned importmap
entries by hand would bypass provenance recording.

## Other modes

- `Mapper` in dev mode serves sources directly, re-reading and
  re-hashing on every request with `no-cache` + ETag revalidation -
  edits show up on reload with no compile step and no watcher.
- `NewSource` wraps that dev mode behind the `Server` interface,
  which `Build`'s `Compiled` also satisfies, so an application can
  switch between live source serving and compiled assets on an
  `Options` value.
- `Compile` materializes the hashed tree plus a `manifest.json` to a
  directory for CDN workflows that want files on disk; `Mapper` in
  prod mode resolves URLs from that manifest.

## Errors

`Build`, `Check`, and `Compile` fail loudly and completely: an invalid root,
two assets compiling to the same output name, a malformed `importmap.json`, or
an importmap entry naming a missing asset all abort with a message naming the
culprit. Missing relative JS and CSS imports are also rejected during
planning, before any output is served or published.
`ErrAssetNotFound` reports unknown logical paths at lookup time;
template helpers return errors that surface as execution errors.
