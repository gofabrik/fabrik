# assetmapper

Frontend assets for Go without Node or a bundler: content-hashed URLs
for immutable caching, ES modules with importmaps, JS / CSS reference
rewriting, and vendoring from jspm.io. Stdlib only.

## The model

Assets are plain files in a directory - CSS, ES modules, images,
fonts. The library maps each logical path (`app.css`) to a
content-hashed public URL (`/assets/app-4c9d02ef.css`), rewriting the
references inside JS and CSS (`import "./nav.js"`, `url("bg.png")`,
`@import`) so the whole graph is hash-addressed and cacheable forever.
JavaScript stays standard ES modules the browser runs directly; bare
specifiers like `import htmx from "htmx"` resolve through an
importmap rendered into the page.

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
across replicas. Only content that rewriting changed is held in
memory; images and fonts serve straight from the embedded source FS.

`Handler` owns its prefix stripping - no `http.StripPrefix`. It
serves GET and HEAD (405 otherwise), answers `If-None-Match` with
304, and sets `Cache-Control: public, max-age=31536000, immutable`,
which is safe because every served name embeds its content hash.

`Check(roots, im, opts...)` runs the same pipeline and reports the
error `Build` would, without keeping the result - wire it into CI so
a broken reference or importmap fails the build, not the deploy.

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
and the entrypoint tag. CSP variants (`importmap_nonce`,
`module_preload_links_nonce`) take a nonce for
`script-src 'nonce-...'` policies.

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

Vendored files are ordinary assets afterwards - committed, embedded,
hashed, importmap-resolved. There is no lockfile and no install step
on other machines: the files themselves are in the repository.

The same flow works without the CLI. Manually: drop the file at
`assets/vendor/htmx.org.js` and add `"htmx.org": {"version": "2.0.3"}`
to `importmap.json` (a `"version"` entry resolves to
`vendor/<name>.js` by convention). Programmatically: the `Vendor` type
with a `PackageResolver` (jspm.io shipped, others pluggable) is what
the CLI calls.

## Other modes

- `Mapper` in dev mode serves sources directly with lazy hashing,
  `no-cache` + ETag revalidation, and cache invalidation hooks for
  file watchers - edits show up on reload with no compile step.
- `Compile` materializes the hashed tree plus a `manifest.json` to a
  directory for CDN workflows that want files on disk; `Mapper` in
  prod mode resolves URLs from that manifest.

## Errors

`Build` and `Check` fail loudly and completely: an invalid root, a
dependency cycle among modules, two assets compiling to the same
output name, a malformed `importmap.json`, or an importmap entry
naming a missing asset all abort with a message naming the culprit.
`ErrAssetNotFound` reports unknown logical paths at lookup time;
template helpers return errors that surface as execution errors.
