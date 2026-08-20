# authn/session

Session-backed authentication for [authn](../README.md). Login stores
claims, middleware authenticates later requests, and logout destroys
the session.

```go
sessions, _ := session.New(session.Config{ /* ... */ })
auth, _ := sessionauth.New(sessions, sessionauth.Options{})

login := func(w http.ResponseWriter, r *http.Request) {
	claims := &authn.ClaimSet{Subject: "alice", Roles: []string{"admin"}}
	if err := auth.Login(r.Context(), claims); err != nil {
		http.Error(w, "login failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

handler := sessions.Middleware(auth.Middleware(mux))
_ = login
```

`Manager` combines the session `Registry`, `Promote`, `Destroy`, and
`UserID` operations. `*session.Manager` satisfies it.

## Semantics

### Login

`Login(ctx, claims)` requires a subject, promotes it to the session
user ID, and rotates the session ID. Old-ID revocation is best-effort:
if the store fails, rotation succeeds but the old ID remains valid
until expiry, and the session manager logs the failure.

Login state commits when the response starts, so call `Login` first.
If saving claims after promotion fails, `Login` destroys the session
on a best-effort basis to prevent stale claims from authenticating the
new identity.

The stored snapshot omits `exp` and `nbf` because session expiry
controls validity. Other claim changes take effect at the next login.
Immediate revocation requires revoking the user's sessions with
`sessions.RevokeByUser(ctx, userID)` or using short session lifetimes.

### Logout

`Logout` destroys the session and is a no-op without session state.
State written afterward belongs to a new anonymous session.

### Authentication

`Authenticate` returns stored claims or `(nil, nil)` for a session
without claims or a request without session state. Store and decode
failures are errors. A session user ID that differs from the stored
subject is also an error because the session identity is authoritative.

Middleware attaches session claims only when the request has none.
Read failures are logged with `Options.Logger` (default
`slog.Default()`) and treated as anonymous; direct `Authenticate`
calls receive the error.
