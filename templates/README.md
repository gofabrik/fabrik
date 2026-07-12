# templates

HTML page templates for Go with a **layout-by-section** model. Rendering goes
through `http.ResponseWriter`. Dependencies are limited to stdlib
`html/template` and `io/fs`.

## The model

Pages live under section directories. Each section may declare its own
`_layout.html` and section-local partials (`_*.html`). The conventional
`_default` section provides shared layouts and partials.

```
templates/
├── _default/
│   ├── _layout.html
│   ├── _flash.html
│   ├── home.html
│   └── monitors.html
└── public/
    ├── _layout.html
    └── status.html
```

A section without a layout uses `_default`'s; section partials shadow
`_default`'s on filename collision. A page's key is its bare basename in
`_default`, `section/basename` anywhere else.

## Usage

```go
//go:embed all:templates
var files embed.FS

set, err := templates.Load(files, "templates")
if err != nil {
	log.Fatal(err)
}

func home(w http.ResponseWriter, r *http.Request) {
	if err := set.Render(w, "home", data); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}
```

Use the `all:` prefix. Plain `//go:embed templates` excludes `_layout.html`
and underscore-prefixed partials.

`Render` executes the page into a buffer first. An error writes nothing
to the response, leaving status-code policy to the caller. On success the
Content-Type header is set and the HTML is flushed in one write. `Set` is
safe for concurrent use; construct it once at startup.

## Functions

Templates see [DefaultFuncs] (`add`, `sub`) plus any maps passed to Load.
Later maps win; caller maps may override defaults:

```go
set, err := templates.Load(files, "templates", templates.FuncMap{
	"humanizeAge": humanizeAge,
	"add":         myOwnAdd,
})
```

`FuncMap` aliases `html/template.FuncMap`. Helpers can be declared without
importing the stdlib package alongside this one.

## Multiple trees

`LoadSources` unions several trees into one set. Shared layout and partials
can live in one package while other packages own their pages:

```go
set, err := templates.LoadSources([]templates.Source{
	{FS: shared.Templates, Dir: "templates"},
	{FS: web.Templates, Dir: "templates"},
})
```

Every source has section directories inside. Each section belongs to exactly
one source, including `_default`. Fallback works across sources: a page in one
tree can render through another tree's `_default` layout and partials.

## Layered overrides

`LoadLayers` combines ordered layers. Within a layer, sections must still be
disjoint, as in `LoadSources`. Across layers, a later layer's page, partial, or
layout overrides the same-named file from an earlier layer. The union of all
layers is the section.

```go
set, err := templates.LoadLayers([][]templates.Source{
	{{FS: baseUI, Dir: "templates"}},
	{{FS: app.Templates, Dir: "templates"}},
})
```

The overlay can replace one `auth/login.html` while keeping the base's
`auth/register.html`, or override `auth/_layout.html` to reskin the section
without touching a page. Partials parse in layer order, so a later partial wins
its `{{ define }}` even under a different filename. `Set.Overrides()` returns
cross-layer shadows with section, file, kind, and source refs.

Pick by intent:

- `Load` - one tree.
- `LoadSources` - several trees, strict: a section lives in one source.
- `LoadLayers` - ordered layers, where a later layer overrides an earlier one.

## Errors

Load fails loudly and completely: any template parse error, a reference
to an unknown function, a duplicate page key, or a section that has
pages but no resolvable layout aborts with a message naming the file.
`Render` returns an error for an unknown page name or a template execution
failure. Nothing is written to the response in either case.
