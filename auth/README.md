# auth

Authentication primitives for Go HTTP services: claims, context
transport, predicates, and route guards. Authentication schemes live
in separate modules. The package works with `net/http` and any mux.

```go
authenticate := func(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user := r.Header.Get("X-User"); user != "" {
			r = r.WithContext(auth.WithClaims(r.Context(), &auth.ClaimSet{
				Subject: user,
				Roles:   []string{"admin"},
			}))
		}
		next.ServeHTTP(w, r)
	})
}

var g auth.Guard
mux := http.NewServeMux()
mux.Handle("/admin", g.Role("admin")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	c, _ := auth.Claims(r.Context())
	fmt.Fprintf(w, "hello %s", c.Subject)
})))
http.ListenAndServe(":8080", authenticate(mux))
```

## Claims

`ClaimSet` contains the RFC 7519 registered claims (`iss sub aud exp
nbf iat jti`), the RFC 8693 claims `scope` and `client_id`, and the
RFC 9068 claims `roles`, `groups`, and `entitlements`. Unknown JSON
claims are discarded during unmarshal.

`Audience` accepts a string or an array and marshals a single value as
a string. `NumericDate` preserves fractional Unix seconds, marshals
whole values as integers, and rejects magnitudes at or above 2^53.
`ClaimSet.Scopes()` splits the space-delimited scope claim.

## Transport and predicates

Attach claims with `WithClaims(ctx, c)` and read them with
`Claims(ctx)`. Passing nil claims leaves the context unchanged.

- `IsAuthenticated(ctx)`: claims present with a non-empty subject. A
  client-only token has grants but is not an authenticated principal.
- `HasScope(ctx, s)` / `HasRole(ctx, r)`: read the claims as-is,
  subject or not.

## Authenticator

Authentication schemes implement:

```go
type Authenticator interface {
	Authenticate(r *http.Request) (*ClaimSet, error)
}
```

The result is tri-state: `(claims, nil)` for a valid credential,
`(nil, nil)` when no credential for the scheme is present, and
`(nil, err)` for an invalid credential. Credential creation, such as
login or token issuance, is outside this interface.

## Guards

`Guard` builds ordinary `func(http.Handler) http.Handler` middleware.
The zero value is usable:

```go
var g auth.Guard
mux.Handle("/private", g.Authenticated()(private))
mux.Handle("/admin", g.Role("admin")(admin))
mux.Handle("/export", g.Scope("export")(export))
```

A request without an authenticated principal gets 401; a principal
without the required grant gets 403. `OnUnauthenticated` and
`OnForbidden` override those responses, each receiving the original
request:

```go
g := auth.Guard{
	OnUnauthenticated: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}),
}
```
