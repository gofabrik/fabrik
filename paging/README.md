# paging

Page numbers for Go. Turns an untrusted page number and a total count into the
numbers a store query and a set of page controls both need.

## Features

| Feature | What it gives you |
|---|---|
| One value, both jobs | The same `Page` answers `Limit`/`Offset` for the query and `HasPrev`/`Next` for the controls. |
| Clamped, never an error | Page 0, a negative page, or a page past the end lands on a real page, so a stale bookmark still shows results. |
| No overflow | The number is clamped before `Offset` multiplies it, so an absurd page cannot produce a nonsense offset. |
| Holds nothing | No data, no queries, no store coupling. You count the rows; it does the arithmetic. |
| Imports nothing | Arithmetic only. It never sees a request, so it cannot disagree with your router about what a page number is. |

## Install

```bash
go get github.com/gofabrik/fabrik/paging
```

## Usage

```go
total, err := store.Count(ctx, search)
if err != nil {
    return err
}

// Atoi leaves 0 on anything unparseable, which Of clamps to the first page.
requested, _ := strconv.Atoi(r.URL.Query().Get("page"))
page := paging.Of(requested, 10, total)

episodes, err := store.Search(ctx, search, page.Limit(), page.Offset())
```

In the template:

```html
{{ .Page.Total }} results — page {{ .Page.Number }} of {{ .Page.Pages }}

{{ if .Page.HasPrev }}<a href="?page={{ .Page.Prev }}">Previous</a>{{ end }}
{{ if .Page.HasNext }}<a href="?page={{ .Page.Next }}">Next</a>{{ end }}
```

## Page

```go
type Page struct {
    Number int // 1-based, clamped to Pages
    Size   int // items per page, at least 1
    Total  int // items across all pages
    Pages  int // 0 when nothing matched
}

func Of(requested, size, total int) Page

func (Page) Limit() int      // rows to ask for
func (Page) Offset() int     // rows to skip
func (Page) Empty() bool     // nothing matched at all
func (Page) HasPrev() bool
func (Page) HasNext() bool
func (Page) Prev() int       // this page when there is no previous
func (Page) Next() int       // this page when there is no next
func (Page) First() int      // 1-based position of this page's first item
func (Page) Last() int       // ...and its last, short on the final page
```

Read the requested page yourself, from a query string, a path segment, a form
field, or anywhere else. `Of` takes any `int`, including a negative or absurd
one, so there is nothing to check before calling it.

**Nothing matched** gives `Pages == 0` and `Number == 1`, not page 1 of 0. Ask
`Empty()` rather than comparing `Total` yourself.

**A size below 1 becomes 1** and **a negative total becomes 0**, so neither can
divide by zero or invent pages.

## Not included

Anything to do with HTTP. No request parsing, no URL building. A link needs
the caller's other query parameters preserved, and where a page number arrives
from is the caller's business.

Sorting, cursors, and keyset pagination. This is offset paging only; a large
offset is slow in every database, and that limit is the caller's to know.

## Status

Reference code.
