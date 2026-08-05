# web

Typed HTTP responses for Go handlers: request in, response value out,
errors centralized. Sectioned HTML templates load into the renderer the
adapter uses. The request side is a light wrapper; full request typing
belongs to form binding. Zero dependencies.

## Why

`net/http` handlers repeat the same choreography: write an error,
return immediately, set headers before the body, and pick a status
before the first write. Tests often need recorders and byte matching.

`web` handlers return values instead of writing:

```go
func (h *Handlers) Login(req *web.Request) (web.Response, error) {
	user, err := h.Store.UserByEmail(req.Context(), req.FormValue("email"))
	if err != nil {
		return nil, err
	}
	if user == nil {
		return web.Template("auth/login", LoginPage{Error: "invalid credentials"}), nil
	}
	req.SetCookie("session", token, web.CookieSecure())
	return web.Redirect("/account"), nil
}
```

- The compiler enforces one outcome per path. A handler cannot
  half-respond.
- `return nil, err` invokes the configured error handler. By default,
  every error is logged; errors carrying an `HTTPStatus() int` value
  from 400 through 599 use that status, and others produce a plain 500.
- Handlers test as plain functions. Comparable responses compare
  directly (`resp == web.Redirect("/account")`), the rest assert by
  type and fields. No recorder, no byte matching.

**A convenience layer, not a departure from net/http.** `Wrap` produces
a plain `http.HandlerFunc`; standard `func(http.Handler) http.Handler`
middleware applies unchanged; `req.HTTP()` reaches the underlying
request. Typed and plain handlers mix freely, route by route. Adopting
`web` for one handler commits nothing about the next.

## Usage

```go
set, err := web.LoadTemplates(templatesFS, "templates")
if err != nil {
	return err
}
adapter := web.NewAdapter(
	web.WithRenderer(set),             // anything with Render(w, name, block, data) error
	web.WithErrorHandler(onError),     // default: log, then ErrorStatus or 500
)

mux := http.NewServeMux()
mux.HandleFunc("GET /login", adapter.Wrap(h.ShowLogin))
mux.HandleFunc("POST /login", adapter.Wrap(h.Login))
mux.HandleFunc("GET /health", plainHandler)
```

## Responses

| Value | Behavior |
|---|---|
| `web.Template(name, data)` | renders a named template via the renderer, buffered: a failed render reaches the error handler with nothing written |
| `web.JSON(v)` | `application/json`, buffered; encode errors reach the error handler with nothing written |
| `web.Redirect("/x")` | 303 See Other; the URL is sent verbatim, use absolute paths |
| `web.RedirectPermanent("/x")` | 308 |
| `web.Status(code)` | status only, no body byte |
| `web.Text(code, s)`, `web.HTML(code, s)` | small direct bodies |

Template and JSON responses chain; every call returns a copy, so a
shared value is safe from what any handler adds to it:

```go
web.Template("article/form", data).Status(http.StatusUnprocessableEntity)
web.Template("todo/list", todo).Block("row")   // one fragment, not the document
web.JSON(item).Status(http.StatusCreated).Header("Cache-Control", "no-store")
```

## Templates

`LoadTemplates` parses a tree of section directories: files whose names
begin with an underscore are partials shared into every page of their
section, and the `_default` section supplies partials to every other
section. Page names are bare basenames in `_default` and
section-qualified elsewhere (`web/home`). Several trees combine with
`LoadTemplateSources`; a section belongs to exactly one tree.

There is no layout rule. A page and its partials parse into one group,
and rendering executes whichever template in the group is named - the
whole document or one fragment of it. The one convention is the
default: an empty block renders `_layout.html`, the conventional
layout partial, wrapping whatever the page calls `content`.
`WithBlock` replaces that default adapter-wide; `.Block(name)` per
response.

A page declares which of its pieces are addressable from outside by
naming them with the `region/` prefix (`{{define "region/results"}}`);
`Templates.Region` resolves such a name and nothing else.

The `Renderer` is a one-method interface
(`Render(w, name, block string, data any) error`); any template system
satisfying it plugs in, and each renderer owns what an empty block
means.

## One adapter per response surface

An adapter carries one renderer and one error handler, so give each
surface its own. Pages want a rendered error page; an API wants a JSON
error body:

```go
pages := web.NewAdapter(web.WithRenderer(set), web.WithErrorHandler(errorPage))
api := web.NewAdapter(web.WithErrorHandler(jsonError))

mux.HandleFunc("GET /account", pages.Wrap(h.Account))
mux.HandleFunc("GET /api/items", api.Wrap(h.Items))
```

Adapters are cheap and stateless.

## Contracts

- **Error path**: a returned error, a respond error, and the `nil, nil`
  programming error (surfaced as `web.ErrNilResponse`) all reach the
  configured `ErrorHandler`. It is net/http-native
  (`func(w, r, err)`), so a failing error page cannot recurse.
- **Recorded state is success-only**: `SetHeader`/`SetCookie`/
  `ClearCookie` on the request record per-call response state. It
  applies only when the handler returns a response. A failed handler's
  cookies are dropped, never attached to the error response.
- **Ordering**: recorded headers apply first (Set semantics), cookies
  append, then the response runs. A Response setting the same header
  wins. Every built-in decides status and headers before writing any
  body byte.
