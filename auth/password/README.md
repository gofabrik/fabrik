# auth/password

Email + password authentication. Verifies credentials against your
user store and hands a finished `auth.Identity` to a sink (session,
or any other identity writer). Exposes three HTTP handlers and mounts
nothing - you name the paths and attach middleware.

```go
ph, err := password.New(userStore, sink, password.Options{})

mux.HandleFunc("POST /login", ratelimited(ph.Login))
mux.HandleFunc("POST /register", ratelimited(ph.Register))
mux.HandleFunc("POST /logout", ph.Logout)
```

Rate limiting and CSRF are the app's job, attached at registration
like any route middleware - a login route without throttling is
bounded only by bcrypt latency.

## You supply

- **`Store`** - `LookupByEmail` and `Create` over your user table.
  Email normalization (case folding) is the store's contract, so
  lookup, create, and the uniqueness constraint agree.
- **`Sink`** - `Login(ctx, id)` / `Logout(ctx)`. `*authsession.Authenticator`
  satisfies it; password never imports `session`.

## Behavior worth knowing

- **No enumeration.** Unknown email and wrong password are one 401;
  a taken email on register is one 401. Unknown-email login still
  runs a same-cost bcrypt verify against a dummy hash, so response
  time does not leak whether the email exists.
- **Operational failures are 500, not 401.** A store outage or
  hashing failure is a server error; only credential rejections are
  401. Custom `OnFailure` handlers branch on the typed `*Error` and
  its `Op` (`errors.As`), never on string prefixes.
- **Hooks return errors.** `OnSuccess` and `OnRegistered` do fallible
  post-auth work (flash, redirect, welcome email) and return the
  error before writing the response; the provider routes it as an
  `OpHook` 500.
- **Register is always callable.** An app that does not want
  registration simply does not route the handler.
