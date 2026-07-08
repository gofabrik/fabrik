# router

Thin routing library on top of `http.ServeMux`.

The router adds middleware, route groups, mounting, and custom 404/405 handlers.
Routes register directly on one `ServeMux`, so dispatch, path parameters,
redirects, and `Allow` headers keep standard library behavior.

Standard library only. No third-party dependencies.

## Usage

```go
r := router.New()

r.Use(logger, recoverer)

r.Get("/health", health)
r.Get("/users/{id}", show)

r.Route("/api", func(r *router.Scope) {
	r.Use(auth)
	r.Get("/me", me)
	r.Post("/posts", create)
})

http.ListenAndServe(":8080", r)
```

`New` returns the servable `*Router`. `With`, `Group`, and `Route` return
registration-only `*Scope` values.

## API

- `Get`/`Post`/`Put`/`Patch`/`Delete`/`Head`/`Options(pattern, handler)` and
  `Method(verb, pattern, handler, mw...)`.
  `verb` may be any method token, including `CONNECT`, `TRACE`, and extension methods.
- `Handle`/`HandleFunc(pattern, handler)` - method-agnostic registration.
- `MethodHandle(method, pattern, handler)` - `Method` for an `http.Handler`.
- `Router.NotFound(h)`/`Router.MethodNotAllowed(h)` - custom handlers for routing
  misses; `h` is an `http.HandlerFunc`.
- `Router.Use(mw...)` - global middleware; wraps every request, including 404/405.
- `Scope.Use(mw...)` - scoped middleware; applies to routes registered on the scope
  afterward.
- `With(mw...)` - a `*Scope` that applies `mw` to the routes registered on it.
- `Group(fn)` - a scope sharing the current middleware; `fn` takes a `*Scope`.
- `Route(prefix, fn)` - a scope with an added path prefix; `fn` takes a `*Scope`.
- `Mount(prefix, sub)` - flattens a `*Router`'s routes under `prefix` (middleware
  folded, params in the prefix preserved). The subrouter is copied as it is at the
  call, so configure it fully before mounting.
- `Router.Routes()` - the registered method/pattern pairs.
- `Router.Walk(fn)` - visit each route with its raw and middleware-wrapped handler.

Path parameters use `req.PathValue("id")`. There is no params API and nothing is
stored in the request context.

`Mount` flattens routes. A mounted subrouter's `Use` middleware runs only for
matched routes, not for 404/405 under the mount prefix. Its `NotFound` and
`MethodNotAllowed` handlers are not copied. To attach something that owns its own
responses, register it as a handler and strip the prefix yourself:

```go
r.Handle("/static/", http.StripPrefix("/static", fileServer))
r.Handle("/api/", http.StripPrefix("/api", subApp))
```

## Behavior

Routing follows `http.ServeMux` rules:

- Precedence is most-specific-pattern-wins, not registration order.
- A trailing slash (`/files/`) matches the subtree and redirects `/files` to
  `/files/`. Use `{$}` to match a path exactly.
- `{name}` matches one segment; `{name...}` matches the rest of the path.
- 404 and 405 are the standard library's, including a correct `Allow` header
  unioned across every pattern that matches the path. `OPTIONS` with no explicit
  route returns 405 with `Allow`.
- Invalid or conflicting patterns panic at registration.

Patterns are paths and must begin with `/`; host-qualified ServeMux patterns (such
as `example.com/`) are not supported.

Build the router fully before serving. Registration after first serve panics, but
the guard is not a concurrency primitive.

## Custom 404 / 405

`NotFound` and `MethodNotAllowed` take ordinary handlers. The status defaults to
404/405, so writing only a body is enough. Call `WriteHeader` to override it:

```go
r.NotFound(func(w http.ResponseWriter, req *http.Request) {
	templates.ExecuteTemplate(w, "404.html", map[string]any{"Path": req.URL.Path})
})

r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
	templates.ExecuteTemplate(w, "405.html", map[string]any{"Allow": w.Header().Get("Allow")})
})
```

Only routing misses reach these hooks. Matched handlers that write 404 are left
alone, and `MethodNotAllowed` receives the `Allow` header from `ServeMux`. Leaving
a hook unset keeps the standard library response for that status.

The error handler's `http.ResponseWriter` is wrapped to apply the default status,
so use `http.ResponseController(w)` for optional capabilities such as
`http.Flusher`.

Setting a hook adds one classification match per request. Routers without hooks
dispatch straight through `ServeMux`.

## Benchmarks

Dispatch over a shared 25-route REST table, Apple M1, best of `-count=8`:

| Scenario | `http.ServeMux` | chi v5.3.0 | router |
| --- | --- | --- | --- |
| static | 107 ns · 0 B · 0 allocs | 217 ns · 368 B · 2 allocs | 108 ns · 0 B · 0 allocs |
| 1 param | 159 ns · 16 B · 1 alloc | 386 ns · 704 B · 4 allocs | 163 ns · 16 B · 1 alloc |
| 2 params | 339 ns · 48 B · 2 allocs | 524 ns · 704 B · 4 allocs | 335 ns · 48 B · 2 allocs |

The wrapper tracks `ServeMux` without extra allocations.

Reproduce:

```sh
cd bench
GOWORK=off go test -bench . -benchmem
```
