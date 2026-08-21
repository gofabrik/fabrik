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
