# templates

Sectioned HTML templates for Go with a **layout-by-section** model, built on
`html/template`. Rendering writes to any `io.Writer` and does not set HTTP
headers.

## The model

Templates live under section directories. Each section may declare a layout
(`_layout.html`) and partials (`_*.html`). The `_default` section provides
fallback layouts and partials for every other section.

```
templates/
├── _default/
│   ├── _layout.html
│   ├── _flash.html
│   ├── home.html
│   └── monitors.html
├── public/
│   ├── _layout.html
│   └── status.html
└── errors/
    ├── 404.html
    └── 405.html
```

A section without a layout uses `_default`'s; section partials shadow
`_default` partials with the same filename. Names are bare basenames in
`_default` and section-qualified elsewhere, without the extension, as in
`public/status`.

Every template requires a layout and defines `content`. Non-HTML files are
ignored, so the same tree can hold files parsed by other engines.

## Usage

```go
//go:embed all:templates
var files embed.FS

set, err := templates.Load(files, "templates")
if err != nil {
	log.Fatal(err)
}

if err := set.Render(w, "public/status", data); err != nil {
	log.Fatal(err)
}
```

Use the `all:` prefix. Plain `//go:embed templates` excludes layouts and
underscore-prefixed partials.

`Render` buffers template execution. Lookup and execution errors write
nothing; writer errors may leave partial output. `Render` does not set HTTP
headers. `Set` is safe for concurrent use; construct it once at startup.

## Functions

Templates see [DefaultFuncs] (`add`, `sub`) plus any maps passed to Load.
Later maps win; caller maps may override defaults:

```go
set, err := templates.Load(files, "templates", templates.FuncMap{
	"humanizeAge": humanizeAge,
	"add":         myOwnAdd,
})
```

`FuncMap` aliases `html/template.FuncMap`.

## Multiple trees

`LoadSources` unions several trees into one set. Shared layout and partials
can live in one package while other packages own their templates:

```go
set, err := templates.LoadSources([]templates.Source{
	{FS: shared.Templates, Dir: "templates"},
	{FS: web.Templates, Dir: "templates"},
})
```

Every source has section directories inside. Each section belongs to exactly
one source, including `_default`. Fallback works across sources: a template
in one tree can render through another tree's `_default` layout and partials.

## Errors

Load rejects parse errors, unknown functions, and sections with templates but
no resolvable layout. Parse errors identify the source file. `Render` returns
lookup and execution errors without writing to the writer.
