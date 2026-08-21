# auth/store

Package `store` persists identities and their password credentials.
An identity anchors an account; credentials attach to it per scheme.
Every implementation satisfies the [password](../password/README.md)
`Store` and `Rehasher` interfaces through `Lookup` and `UpdateHash`,
so a store plugs directly into a `password.Verifier`.

```go
s := store.NewMemoryStore(store.MemoryOptions{})

_, err := s.CreateIdentity(ctx, "alice", store.IdentityClaims{Roles: []string{"admin"}})
hash, _ := password.Argon2id{}.Hash("open sesame")
err = s.CreatePassword(ctx, "alice", "alice@example.com", hash)

verifier, _ := password.New(password.Config{Store: s})
claims, err := verifier.Authenticate(ctx, email, submitted)
```

## Identities

`CreateIdentity` creates an active identity under a caller-supplied
opaque id of at most `MaxIdentityIDLen` (191) bytes; ids compare
byte-exact. `SetIdentityStatus` switches future logins on or off
without touching live sessions; revoke those through the session
manager. `DeleteIdentity` is idempotent and removes the identity's
credentials with it.

Identity claims are the identity-owned authorization data: roles,
groups, and entitlements. Token and request claims (issuer, audience,
scope, ...) describe a grant, not a person, and are never persisted.
`Lookup` composes the returned `password.Credential` with `Subject`
set to the identity id and only the identity claims filled in.

## Password credentials

`CreatePassword` attaches a credential to an identity that has none;
it never updates an existing row. `SetPassword` replaces the
identity's credential, hash and email both. Emails are normalized
with `password.NormalizeEmail` and compare after normalization; an
email owned by a different identity is `ErrEmailTaken`, which
outranks `ErrExists` so create-if-absent seeding fails loudly on a
misdirected email instead of silently tolerating it.

Retry-safe seeding is `CreateIdentity` tolerating `ErrExists`
followed by `CreatePassword` tolerating `ErrExists` only - an
interrupted first run recovers on the next run, and nothing can
overwrite a live credential.

## The verifier seam

`Lookup` and `UpdateHash` treat invalid input and disabled identities
exactly like unknown emails (`password.ErrNotFound`), preserving the
verifier's uniform-failure contract. `UpdateHash` compares the stored
hash byte-exact and returns `password.ErrHashChanged` when it is no
longer current, so rehashing cannot overwrite a concurrent password
change.

## Implementations

`MemoryStore` is the process-local reference implementation for
development, tests, and fixed account sets. `storetest.Run` is the
conformance suite an implementation must pass.

### Database-backed stores

Each leaf exposes `Store`, `Options{AutoCreate, Now}`,
`New(db, opts)`, `Schema()`, and `SchemaStatements()`. Apply the
schema through migrations in production, or pass `AutoCreate: true`
in development and tests; `SchemaStatements()` returns the ordered
statements for drivers that reject multi-statement text.

**SQLite** (`store/sqlite`):

```go
import "github.com/gofabrik/fabrik/auth/store/sqlite"

db, err := sql.Open("sqlite", "file:app.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_txlock=immediate")
s, err := sqlite.New(db, sqlite.Options{AutoCreate: true})
```

The foreign_keys pragma is required for cascade delete and the
immediate txlock serializes the store's write transactions.

**PostgreSQL** (`store/postgres`):

```go
import "github.com/gofabrik/fabrik/auth/store/postgres"

db, err := sql.Open("pgx", "postgres://user:pass@host/db?sslmode=disable")
s, err := postgres.New(db, postgres.Options{AutoCreate: true})
```

Identity ids, emails, and hashes are stored as BYTEA for byte-exact
comparison; conflict-checking writes serialize on a
transaction-scoped advisory lock.
