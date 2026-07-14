# session

Typed HTTP sessions for Go. Declare one struct, and the manager
persists it per visitor. Standalone: net/http and any mux, no
framework required.

```go
type Session struct {
	Locale   string
	CartSize int
}

sessions, err := session.New[Session](session.Config{
	Store:          session.NewMemoryStore(),
	Token:          session.Cookie{Name: "session", HttpOnly: true, SameSite: http.SameSiteLaxMode},
	AbsoluteExpiry: 24 * time.Hour,
	IdleExpiry:     time.Hour,
})

mux := http.NewServeMux()
mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
	s, _ := sessions.Get(r.Context())
	s.CartSize++
	_ = sessions.Save(r.Context(), s)
})
http.ListenAndServe(":8080", sessions.Middleware(mux))
```

One value carries everything:

| | |
|---|---|
| Data | `Get`, `Has`, `Save`, `Update`, `Clear` - all in terms of your struct |
| Lifecycle | `Promote` (login), `Destroy` (logout), `Renew`, `SID`, `UserID` |
| Out-of-band | `Load`, `UpdateSID`, `ClearSID`, `DestroySID`, `ListForUser`, `RevokeAllForUser` |

## Semantics

- **Writes stage, then commit once.** `Save` and `Clear` mark the
  request dirty; middleware commits at response start. `Update` is
  the immediate CAS read-modify-write path for durable mid-request
  writes.
- **Your struct persists through `encoding/json`.** Exported,
  JSON-marshalable fields round-trip; unexported fields silently do
  not persist; a value that fails to encode or decode errors at
  `Save`/`Get`/`Update`, not at construction.
- **Reads never mint.** A session exists once something writes: a
  staged `Save` or `Promote` mints at commit; a sessionless
  `Update` mints immediately. `Get` on a fresh visitor is your
  struct's zero value, no error.
- **Dead tokens clean themselves up.** A request whose token names
  nothing proceeds sessionless, and the first request that touches
  session state clears the dead cookie at commit. Requests that
  never touch session state never touch the store.
- **Login rotates.** `Promote` stages an SID rotation with the
  identity change. Rotation preserves the absolute deadline; only
  `Destroy`-then-write gets a fresh session.
- **After response start**, staged mutators return
  `ErrAlreadyCommitted`; `Update` on an established session still
  works (state only - no commit means no token refresh).

## Tokens

The `Token` interface carries the session ID: `Read`, `Write`,
`Clear`.

**`Cookie`** - browser transport.

| Field | Default | Notes |
|-------|---------|-------|
| `Name` | `"sid"` | |
| `Path` | `"/"` | |
| `Domain` | unset | |
| `Secure` | `false` | Opt in for production |
| `HttpOnly` | `false` | Opt in for production |
| `SameSite` | unset | `http.SameSiteLaxMode` is the usual choice |

The zero value is not production-safe. Set `HttpOnly`, `Secure`, and
usually `SameSite` before deploying. The quickstart omits `Secure` so
it works on plain-HTTP localhost.

`Expires`/`MaxAge` track the earlier of the session's absolute and
idle expiry, refreshed on commits that write. The ID is an opaque,
high-entropy lookup key.

**`Bearer`** - header transport.

| Field | Default |
|-------|---------|
| `ReadHeader` | `"Authorization"` (scheme `"Bearer"`, case-insensitive) |
| `WriteHeader` | `"X-Session-Token"` |

Bearer ignores token expiry (headers have no expiration mechanism);
production API clients own their token lifecycle through explicit
endpoints.

**`Multi`** - composes transports: first `Read` wins, `Write` and
`Clear` fan out. Composition mistakes (empty `Multi`, nil members)
are construction errors at `New`.

## Stores

`Store` is three methods: `Load`, `Save`, `Delete`, moving opaque
payload bytes under CAS versioning, with optional capabilities
(`TTLBumper`, `UserIndexer`, `Scanner`, `Sweeper`). Two
implementations ship, both fully capable: `MemoryStore`
(process-local, zero config) and `SQLiteStore` (database/sql; bring
your own driver). `SQLiteStore`
bootstraps its schema with `SQLiteOptions{AutoCreate: true}`, or
hand `SQLiteSchema()` to your migration tool; open the DB with a
busy_timeout pragma and `Sweep` from a scheduler near the
idle-expiry cadence.

The `storetest` package is the conformance suite; every store
implementation runs it:

```go
func TestMyStore(t *testing.T) {
	storetest.Run(t, func() session.Store { return NewMyStore() })
}
```

## For libraries (advanced)

A reusable library that needs private session data never learns the
app's type. It declares a typed key once and registers it against
the sealed `Registry` view of the same manager the app holds:

```go
package csrf

type data struct{ Token string }

var key = session.NewKey[data]("github.com/you/csrf")

type CSRF struct{ cell *session.Handle[data] }

func New(m session.Registry) (*CSRF, error) {
	h, err := session.Use(m, key)
	if err != nil {
		return nil, err
	}
	return &CSRF{cell: h}, nil
}
```

The library's data coexists with the app's in one session record and
commits in the same write. An unexported key keeps the cell private.
`Handle` mirrors the manager's data and out-of-band operations for
its own cell. App code needs none of this section.
