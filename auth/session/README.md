# auth/session

The bridge between `auth` and the core `session` library. It is the
one place identity is read out of a session, and the sink login
backends (`auth/password`, and others later) write identity through.

Its import path is `auth/session` but its package is named
`authsession`, so it sits next to the core `session` package with no
alias on either.

```go
import (
	"github.com/gofabrik/fabrik/session"
	"github.com/gofabrik/fabrik/auth/session" // package authsession
)

sessions, _ := session.New[AppSession](session.Config{...})
sa, _ := authsession.New(sessions)

ph := password.New(userStore, sa, password.Options{}) // sa is the sink
chain := auth.Chain(sa)                                // sa is the reader
```

`authsession.New` takes the sealed `session.Registry` +
`session.Lifecycle` view of the manager, so it never learns the
app's session type.

## How identity is stored

Following the session library's one-source rule:

- The session's `UserID` is the canonical auth key,
  `provider + ":" + subject` - so `password:123` and `forward:123`
  are different users in `ListForUser` / `RevokeAllForUser`. Use
  `authsession.UserKey(provider, subject)` to build it for admin
  flows.
- The non-id claims (Email, Name, Provider) live in one private
  cell this package owns - not one per backend, so a cross-backend
  re-login overwrites cleanly and never leaves a stale provider.
- On read, the cell's `{Provider, Subject}` is checked against the
  `UserID`; a mismatch fails closed as unauthenticated, so a
  re-login or logout recovers rather than locking the session out.

`Login` stages the cell write and `Promote` into the request's
single commit. `Logout` is the session's `Destroy`. A missing
session middleware surfaces loudly through `Required` (the chain's
500), not as a silent all-anonymous site.
