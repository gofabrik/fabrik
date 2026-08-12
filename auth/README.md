# auth

Package auth carries typed request identities through contexts. Middleware
resolves identity, and handlers read it with `From` or `IsAuthenticated`.
Standalone: net/http and any mux, no framework required.

```go
func identityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var id *auth.Identity
		if u, ok := lookup(r); ok { // cookie, session, token - your call
			id = &auth.Identity{Subject: u.ID, Issuer: "local", Name: u.Name, Roles: u.Roles}
		}
		next.ServeHTTP(w, r.WithContext(auth.NewContext(r.Context(), id)))
	})
}

mux.HandleFunc("/account", func(w http.ResponseWriter, r *http.Request) {
	if !auth.IsAuthenticated(r.Context()) {
		http.Error(w, "login required", http.StatusUnauthorized)
		return
	}
	id, _ := auth.From(r.Context())
	fmt.Fprintf(w, "hello %s", id.Name)
})
http.ListenAndServe(":8080", identityMiddleware(mux))
```

Tests, jobs, and CLI code attach and read the same way with `NewContext` and
`From`.

## Claims

`Identity` fields use standardized claim names and meanings without
representing a JWT:

| Field | Claim | Origin |
|---|---|---|
| `Subject` | `sub` | RFC 7519 registered claim |
| `Issuer` | `iss` | RFC 7519 registered claim |
| `Name` | `name` | OIDC standard claim |
| `Email` | `email` | OIDC standard claim |
| `EmailVerified` | `email_verified` | OIDC standard claim |
| `AuthTime` | `auth_time` | OIDC (ID token) |
| `Scope` | `scope` | OAuth 2.0 (RFC 6749 section 3.3; RFC 9068) |
| `Roles` | `roles` | RFC 9068 (SCIM-derived) |

`(Issuer, Subject)` identifies an identity because `Subject` is unique only
within its issuer. An empty `Issuer` supports single-authority applications
but risks cross-issuer collisions; the session method rejects it.

`AuthTime` is when the user last authenticated, not when the identity was
attached to a request; a re-authentication refreshes it.

Token-transport claims (`exp`, `nbf`, `iat`, `jti`, `aud`, `azp`, `nonce`,
and `at_hash`) are absent because authentication already evaluates credential
validity.

`Extra` carries application-defined claims. Typed and token-transport claim
names are stripped when attaching an identity.

## Anonymous requests

An empty `Subject` means unauthenticated. A true return from `From` indicates
only that a carrier is present; it may represent a recognized anonymous
client. Use `IsAuthenticated` for access checks.

## Authenticators

A reusable identity source implements:

```go
type Authenticator interface {
	Authenticate(r *http.Request) (*Identity, error)
}
```

Returning `nil, nil` means the method abstains. Errors leave the request
outcome to the caller.

## Placement

Attach on every request. Passing nil to `NewContext` clears an earlier
identity, and later attachments replace earlier ones. Identity middleware
must run inside middleware that provides its inputs and outside middleware
that consumes identity.

On lookup failure, middleware may clear the identity to continue anonymously
or reject the request.

## Snapshots

The carrier is a per-request snapshot. Changing a session does not update it;
the next request observes the new state. After logout, clear the carrier
before continuing:

```go
r = r.WithContext(auth.NewContext(r.Context(), nil))
```

## Mutability

`NewContext` and `From` copy `Identity`, `Roles`, and the `Extra` map. Values
nested inside `Extra` remain shared and should be treated as read-only or
copied by callers.

## Status

Carrier, authenticator contract, and the session method. Login flows and
composition build on them separately.
