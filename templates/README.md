# templates

Sectioned HTML and text templates for Go with a **layout-by-section** model.
`*.html` files use `html/template`; `*.txt` files use `text/template`.
Rendering writes to any `io.Writer` and does not set HTTP headers.

## The model

Templates live under section directories. Each section may declare layouts
(`_layout.html`, `_layout.txt`) and partials (`_*.html`, `_*.txt`). The
`_default` section provides fallback layouts and partials for each format.

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
└── mail/
    ├── _layout.txt
    ├── welcome.html
    └── welcome.txt
```

A section without a layout uses `_default`'s; section partials shadow
`_default` partials with the same filename. Names are bare basenames in
`_default` and section-qualified elsewhere. HTML names omit the extension;
text names retain it, as in `mail/welcome.txt`.

HTML templates require a layout and define `content`. Each text file is a
complete template body. A resolved `_layout.txt` wraps the body through
`{{ template "content" . }}`; without one, the body renders directly. Text
bodies cannot declare named templates; put `define` and `block` actions in
partials.

## Usage

```go
//go:embed all:templates
var files embed.FS

set, err := templates.Load(files, "templates")
if err != nil {
	log.Fatal(err)
}

var body bytes.Buffer
if err := set.Render(&body, "mail/welcome.txt", data); err != nil {
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

`FuncMap` aliases `html/template.FuncMap`. Both formats use the same functions.
Trusted HTML value types render unescaped in text templates; use a
string-returning helper for values intended for both formats.

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

Load rejects parse errors, unknown functions, duplicate names, HTML templates
without a layout, and named definitions in text bodies. Parse errors identify
the source file. `Render` returns lookup and execution errors without writing
to the writer.
