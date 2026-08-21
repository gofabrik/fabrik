# auth/store

Package `store` persists identities and password credentials through
an API compatible with [password](../password/README.md) `Store` and
`Rehasher`.

```go
s := store.NewMemoryStore(store.MemoryOptions{})

_, err := s.CreateIdentity(ctx, "alice", store.IdentityClaims{Roles: []string{"admin"}})
hash, _ := password.Argon2id{}.Hash("open sesame")
err = s.CreatePassword(ctx, "alice", "alice@example.com", hash)

verifier, _ := password.New(password.Config{Store: s})
claims, err := verifier.Authenticate(ctx, email, submitted)
```

## Identities

`CreateIdentity` creates an active identity under an opaque ID of at
most `MaxIdentityIDLen` (191) bytes; IDs compare byte-exact.

`SetIdentityStatus` affects future logins without changing live
sessions, and `DeleteIdentity` idempotently removes an identity and
its credentials.

Identity claims persist roles, groups, and entitlements but exclude
grant-specific token and request claims.

`Lookup` returns a `password.Credential` whose `Subject` is the
identity ID and whose claims contain only persisted identity claims.

## Password credentials

`CreatePassword` adds a credential without replacing one, while
`SetPassword` replaces both its hash and email.

Emails compare after `password.NormalizeEmail`, and `ErrEmailTaken`
takes precedence over `ErrExists` when another identity owns the
email.

For retry-safe seeding, ignore `ErrExists` from `CreateIdentity` and
`CreatePassword`, but do not ignore `ErrEmailTaken`.

## Password verifier compatibility

`Lookup` and `UpdateHash` treat invalid input and disabled identities
like unknown emails by returning `password.ErrNotFound`.

`UpdateHash` uses byte-exact compare-and-swap and returns
`password.ErrHashChanged` rather than overwriting a concurrent
password change.

## Implementations

`MemoryStore` is the process-local reference implementation for
development, tests, and fixed account sets.

`storetest.Run` is the conformance suite for other implementations.

### Database-backed stores

Each database package exposes `Store`, `Options{AutoCreate, Now}`,
`New(db, opts)`, `Schema()`, and `SchemaStatements()`.

Apply the schema through migrations in production or pass
`AutoCreate: true` in development and tests; `SchemaStatements()`
returns ordered statements for drivers that reject multi-statement
text.

**SQLite** (`store/sqlite`):

```go
import "github.com/gofabrik/fabrik/auth/store/sqlite"

db, err := sql.Open("sqlite", "file:app.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
s, err := sqlite.New(db, sqlite.Options{AutoCreate: true})
```

The `foreign_keys` pragma is required for cascade delete, and writes
use `BEGIN IMMEDIATE` independently of the DSN.

**PostgreSQL** (`store/postgres`):

```go
import "github.com/gofabrik/fabrik/auth/store/postgres"

db, err := sql.Open("pgx", "postgres://user:pass@host/db?sslmode=disable")
s, err := postgres.New(db, postgres.Options{AutoCreate: true})
```

Identity IDs, emails, and hashes are stored as BYTEA for byte-exact
comparison; conflict-checking writes serialize on a
transaction-scoped advisory lock.

**MySQL / MariaDB** (`store/mysql`):

```go
import "github.com/gofabrik/fabrik/auth/store/mysql"

db, err := sql.Open("mysql", "user:pass@tcp(host:3306)/db?parseTime=true")
s, err := mysql.New(db, mysql.Options{AutoCreate: true})
```

Identity IDs and emails are stored as VARBINARY and hashes as
LONGBLOB, so no collation can fold case or trailing spaces; write
races resolve through the server's duplicate-key reporting.
