# session

Typed HTTP sessions for Go. Declare one struct, and the manager
persists it per visitor. Standalone: net/http and any mux, no
framework required.

```go
type Session struct {
	Locale   string
	CartSize int
}

sessions, err := session.New(session.Config{
	Store:          session.NewMemoryStore(),
	Token:          session.Cookie{Name: "session", HttpOnly: true, SameSite: http.SameSiteLaxMode},
	AbsoluteExpiry: 24 * time.Hour,
	IdleExpiry:     time.Hour,
})

mux := http.NewServeMux()
mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
	s, _ := sessions.Get[Session](r.Context())
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
- **The first typed access pins your struct.** `New` takes no type
  parameter; the first typed app-data operation requires a struct and
  fixes its type for the manager, with pin errors preceding
  request-scoped errors.
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
(`TTLBumper`, `UserIndexer`, `Scanner`, `Sweeper`). `MemoryStore` is
process-local and zero config. Database-backed stores live in leaf
packages, all four capabilities fully implemented.

### Database-backed stores

Three database backends are available as leaf packages. Each exposes
`Store`, `Options{AutoCreate, Now}`, `New(db, opts)`, and `Schema()`.

**SQLite** (`session/sqlite`):

```go
import "github.com/gofabrik/fabrik/session/sqlite"

db, err := sql.Open("sqlite", "file:app.db?_pragma=busy_timeout(5000)")
store, err := sqlite.New(db, sqlite.Options{AutoCreate: true})
```

**PostgreSQL** (`session/postgres`):

```go
import "github.com/gofabrik/fabrik/session/postgres"

db, err := sql.Open("pgx", "postgres://user:pass@host/db?sslmode=disable")
store, err := postgres.New(db, postgres.Options{AutoCreate: true})
```

**MySQL / MariaDB** (`session/mysql`):

```go
import "github.com/gofabrik/fabrik/session/mysql"

db, err := sql.Open("mysql", "user:pass@tcp(host:3306)/db?parseTime=true")
store, err := mysql.New(db, mysql.Options{AutoCreate: true})
```

`Schema()` returns the table definition, safe to apply more than once;
apply it through migrations in production, or pass `AutoCreate: true`
in development and tests. `Options.Now` overrides the wall-clock
source for expiry filtering and sweep. Open the SQLite DB with a
busy_timeout pragma, and call `Sweep` from a scheduler near the
idle-expiry cadence.

#### Key-length contract

| Backend    | Maximum SID length |
|------------|---------------------|
| SQLite     | no enforced limit  |
| PostgreSQL | ~2704 bytes (B-tree index limit for incompressible keys) |
| MySQL/MariaDB | 3072 bytes (`VARBINARY(3072)` primary key) |

SIDs exceeding the backend limit are rejected by the database; on
PostgreSQL the ceiling is content-dependent (B-tree compression), so
only sufficiently incompressible SIDs over the limit reliably fail.

On MySQL/MariaDB the indexed `user_id` column is `VARBINARY(191)`: user
IDs longer than 191 bytes are rejected. On PostgreSQL `user_id` is
B-tree indexed and shares the SID column's content-dependent ceiling
(sufficiently incompressible values over ~2700 bytes fail on `Save`).
SQLite enforces no user-id bound. SIDs and user ids are arbitrary bytes
on every backend, including NUL and invalid UTF-8.

#### MySQL/MariaDB notes

`sid` is `VARBINARY(3072)` and `user_id` is `VARBINARY(191)`, so both
compare byte-for-byte. MySQL indexes `user_id` unconditionally because
it does not support the partial index used by SQLite and PostgreSQL.

`BumpTTL` locks the row to distinguish an unchanged match from a missing
or expired SID under MySQL's affected-row semantics.

The `storetest` package is the conformance suite; every store
implementation runs it:

```go
func TestMyStore(t *testing.T) {
	storetest.Run(t, func(t *testing.T) session.Store { return NewMyStore() })
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
