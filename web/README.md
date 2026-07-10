# web

Typed HTTP responses for Go handlers: request in, response value out,
errors centralized. The request side is a light wrapper; full request
typing belongs to form binding. Zero dependencies.

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
		return web.View(LoginPage{Error: "invalid credentials"}), nil
	}
	req.SetCookie("session", token, web.CookieSecure())
	return web.Redirect("/account"), nil
}
```

- The compiler enforces one outcome per path. A handler cannot
  half-respond.
- `return nil, err` is the whole error story: logging and the 500 live
  in one configured place.
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
adapter := web.NewAdapter(
	web.WithRenderer(set),             // anything with Render(w, name, data) error
	web.WithErrorHandler(onError),     // default: slog + plain 500
)

mux := http.NewServeMux()
mux.HandleFunc("GET /login", adapter.Wrap(h.ShowLogin))
mux.HandleFunc("POST /login", adapter.Wrap(h.Login))
mux.HandleFunc("GET /health", plainHandler)
```

## Responses

| Value | Behavior |
|---|---|
| `web.View(page)` | renders `page.Template()` with the page as data, via the renderer; an immutable value, safe to share |
| `web.Template(name, data)` | renders a named template directly |
| `web.JSON{Value: v}` | `application/json`, buffered; encode errors reach the error handler with nothing written; `Status` overrides the 200 default (`web.JSON{Status: 201, Value: v}`) |
| `web.Redirect("/x")` | 303 See Other; the URL is sent verbatim, use absolute paths |
| `web.RedirectPermanent("/x")` | 308 |
| `web.Status(code)` | status only, no body byte |
| `web.Text(code, s)`, `web.HTML(code, s)` | small direct bodies |

`View` pages are structs with a `Template() string` method. Data and
destination in one value:

```go
type LoginPage struct{ Error string }
func (LoginPage) Template() string { return "auth/login" }
```

The `Renderer` is a one-method interface (`Render(w, name, data)
error`); any template system satisfying it plugs in.

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

## Contracts worth knowing

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
