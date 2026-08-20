# authn/password

Package `password` verifies email/password credentials with Argon2id and
returns [authn](../README.md) claims for a session login.

```go
store := password.NewMemoryStore()
hash, _ := password.Argon2id{}.Hash("open sesame")
store.Put("alice@example.com", password.Credential{
	Hash:   hash,
	Claims: authn.ClaimSet{Subject: "alice", Roles: []string{"admin"}},
})

verifier, _ := password.New(password.Config{Store: store})

claims, err := verifier.Authenticate(ctx, email, submitted)
if errors.Is(err, password.ErrInvalidCredentials) {
	// Render the login form again.
}
```

## Hashing

`Argon2id` implements `Hasher` with the OWASP interactive-login defaults:
19 MiB memory, 2 passes, and 1 lane. Each zero field in a custom profile uses
its default independently. PHC strings carry their parameters, so existing
hashes remain verifiable after a profile change and `NeedsRehash` selects them
for rewriting.

Profile and stored-hash bounds are checked before derivation. Comparisons are
constant-time, malformed hashes return `ErrInvalidHash` rather than
`ErrMismatch`, and `MaxPasswordLen` (512) bounds password inputs.

## Verification

`Authenticate(ctx, email, password)`:

- Unknown email, wrong password, and empty or oversized input return
  `ErrInvalidCredentials`. Bounds are checked before store or hashing work.
- Unknown emails are compared against a decoy hash at the current profile, so
  they cost the same as current-profile mismatches. Legacy hashes retain their
  encoded cost until rehashed, which remains observable during cost migration.
- Store failures, corrupt hashes, and cancellation remain distinct from invalid
  credentials.
- Successful logins rewrite stale hashes when the store implements `Rehasher`.
  `UpdateHash` uses compare-and-swap so rehashing cannot overwrite a concurrent
  password change. Write failures are logged through `Config.Logger` and do not
  fail the login.
- `Config.MaxConcurrent` (default 4) bounds hashing, including decoy comparisons.
  Use one verifier per process when the bound must be process-wide.

Rate limiting stays outside the library. Apply it before `Authenticate` so
blocked attempts do not reach the hasher. Account keys must use the store's
normalization; `NormalizeEmail` matches `MemoryStore`.

`Authenticate` is not an `authn.Authenticator` because no credential is carried
on each request. Pass its returned claims to a session login.

## Stores

`Store` provides `Lookup(ctx, email) (Credential, error)`, returns `ErrNotFound`
for unknown emails, and owns normalization. `Rehasher` adds optional hash
upgrades. `MemoryStore` normalizes with lowercase plus trim, clones credentials,
and implements compare-and-swap updates for development, tests, and fixed
account sets.
