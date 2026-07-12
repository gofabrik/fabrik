# auth

Pluggable authentication for Go HTTP servers. One identity contract,
composable backends, stdlib-only core. A request either has an
identity or it does not, and handlers never care how it was
established.

```go
chain := auth.Chain(fa, sa) // try each backend in order

mux.Handle("/api/", auth.Required(chain)(apiHandler)) // 401 on miss
mux.Handle("/", auth.Optional(chain)(siteHandler))    // anonymous ok
```

Handlers read the identity from the request context:

```go
id, ok := auth.FromContext(r.Context())
```

## The pieces

- **`Identity`** - the authenticated principal (Subject, Email,
  Name, Provider, Claims). `Subject` is provider-local; never
  compare Subjects across Providers.
- **`Authenticator`** - `Authenticate(*http.Request) (Identity,
  error)`. Return `ErrUnauthenticated` for "no identity here".
- **`Chain`** - composes Authenticators: first non-error wins,
  `ErrUnauthenticated` falls through, any other error
  short-circuits (a store outage never falls through to a weaker
  backend). Put per-request backends (a proxy header) before
  session readers.
- **`Required` / `Optional`** - middleware. Required 401s on a
  miss; Optional passes anonymously; both fail closed (500) on a
  non-sentinel error. Their wire behavior is fixed - for
  redirect-to-login or JSON errors, write your own middleware
  around `Authenticate` and `FromContext`.

The core depends only on the standard library. Backends live in
subpackages and own their own coupling:

- **`auth/password`** - email + password, with login/register/logout
  handlers.
- **`auth/forwardauth`** - trusted reverse-proxy header.
- **`auth/session`** - the bridge to the core `session` library: the
  sink login backends write identity through, and the chain's one
  session-reading authenticator.
