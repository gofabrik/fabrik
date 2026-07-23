# cache

Package `cache` caches computed values by key with TTL: a miss runs
the caller's load function once and stores the result.

```go
counts, err := cache.New[int](cache.NewMemoryStore(cache.MemoryOptions{MaxEntries: 1000}))
if err != nil {
	return err
}

// Later, wherever the value is needed: reads the count from the
// database at most once every ten seconds.
n, err := counts.GetOrLoad(ctx, "greetings", 10*time.Second, func(ctx context.Context) (int, error) {
	return countGreetings(ctx, db)
})
```

## Contract

Cached values must be recomputable because TTL expiry, eviction, or a
restart may remove any entry. Durable state belongs in a database.

A broken cache never stops `GetOrLoad` from producing a value: a
failed read is treated as a miss and the value is loaded, and a failed
write after a successful load is ignored with the value returned
anyway. Both log at Warn through the logger set with `WithLogger`
(default `slog.Default()`). The direct `Get`, `Set`, and `Delete`
calls return cache errors to their caller. Caller cancellation returns `ctx.Err()` without
loading. Cancellation after a successful load skips the write but
still returns the value.

## Loading a missing value

`GetOrLoad(ctx, key, ttl, load)` is the primary API: it returns the
stored value when present and unexpired, and otherwise calls load once
and stores the result for the duration. Concurrent calls for the same
key share that one call instead of each running load. A caller whose
context ends while waiting gets its own context error; if the caller
running load is canceled, one of the others takes over and runs load
itself. Load errors are not cached, and panics propagate to every
caller. Calling `GetOrLoad` for the same key from inside its own load
deadlocks.

`Get`, `Set`, and `Delete` provide direct access. A `ttl <= 0` stores
the entry without expiry.

## Keys and namespaces

Keys are arbitrary strings. `WithNamespace("reports")` prefixes every
key with `reports:` so several typed caches share one store without
collisions; namespaces match `[a-z0-9-]+`.

Caches with different value types can share one store:

```go
store := cache.NewMemoryStore(cache.MemoryOptions{MaxEntries: 10000})
reports, err := cache.New[Report](store, cache.WithNamespace("reports"))
labels, err := cache.New[string](store, cache.WithNamespace("labels"))
```

## Invalidation by versioned keys

There is no group invalidation API; `Delete` removes known keys. For a
value derived from a durable revision such as an `updated_at` column or
content hash, embed the revision in the key:

```go
key := fmt.Sprintf("user/%d/profile@%d", u.ID, u.Revision)
```

A revision change makes the next fetch a miss without deleting the old
key. `MemoryStore` drops old versions as its least recently used
entries once full; SQLite rows persist until `Sweep`. This requires
the caller to know the revision and does not flush arbitrary groups.

## Stores

`MemoryStore` is in-process. `MaxEntries` caps the entry count
(`<= 0` is unbounded); once full, the least recently used entry is
evicted. Expired entries are removed as they are read.

`SQLiteStore` keeps the cache in a SQLite database, so it survives
restarts and is shared by every process using that database file. The
caller registers a driver and should set a busy timeout because the
store does not retry `SQLITE_BUSY`:

```go
db, err := sql.Open("sqlite", "file:app.db?_pragma=busy_timeout(5000)")
if err != nil {
	return err
}
store, err := cache.NewSQLiteStore(db, cache.SQLiteOptions{})
```

`SQLiteSchema()` returns the table definition, safe to apply more
than once; apply it through migrations in production, or pass
`SQLiteOptions{AutoCreate: true}` in development and tests. Reads
never delete: expired rows stay until `Sweep` removes them.

Stores hold `Entry` values (bytes plus absolute expiry) and never read
the wall clock: `Get` and `Sweep` receive the caller's notion of now.
Expiries must be representable as int64 Unix nanoseconds. `Sweep(ctx,
now)` deletes entries expired as of `now`. Applications with unbounded
key sets should run it periodically.

Custom stores are verified with the reusable test suite in the
`storetest` package; see its documentation.
