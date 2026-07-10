# migrations

Forward-only SQL migrations for `database/sql`: plain `NNNN_name.sql`
files, checksummed against drift, applied in version order. SQLite and
Postgres. Stdlib-only core.

## The model

A migration is a file whose name orders it and whose content is
hashed:

```
migrations/
├── 0001_users.sql
├── 0002_sessions.sql
└── 20260507143022_orders.sql
```

`Migrate` applies what is pending and records `(version, name,
checksum, applied_at)` in `schema_migrations`. History is immutable:
editing an applied file is an error (`ErrDrift`), deleting one is an
error (`ErrOrphan`). There are no down migrations. A bad migration
is fixed by the next migration, and the integrity checks depend on
the history being append-only.

## Usage

```go
//go:embed migrations/*.sql
var files embed.FS

fsys, _ := fs.Sub(files, "migrations")
if err := migrations.Migrate(ctx, db, migrations.DialectSQLite, fsys); err != nil {
	log.Fatal(err)
}
```

`Migrate` is idempotent; each migration commits in its own
transaction, and a failure keeps everything already applied
(re-running resumes). A file may contain several statements; the
whole body runs inside the migration's transaction. `Status` reports
every migration as `pending`, `applied`, `drifted`, or `orphan`.

## Streams

Independent packages cannot coordinate a global version sequence, so
migrations group into **streams**: versions order within a stream,
streams are independent, and bookkeeping is keyed by
`(module, version)`:

```go
srcs := migrations.Sources{
	{Module: "auth", FS: auth.Migrations, Dir: "migrations"},
	{Module: "todos", FS: todos.Migrations, Dir: "migrations"},
}
if err := srcs.Migrate(ctx, db, dialect); err != nil { ... }
```

Streams apply in sorted module order, one engine session and one lock
across the whole call. A failing migration skips the rest of its
stream and all later streams; applied work stays.

One hard design rule: **tables that reference each other belong in
one stream.** Streams compose freely only because they are
independent; there is no cross-stream dependency ordering.

A `Migrate` or `Status` call owns the whole `schema_migrations`
table: applied rows in modules not present among the sources are
orphans. Removing a package's migrations without cleaning its rows is
loud, never silent. The table name is fixed by the same decision:
this library assumes it is the only migration tool bookkeeping in the
database. Sharing one database with Rails, goose, or another app's
`schema_migrations` is out of scope, not merely unsupported.

## Concurrency

Concurrent `Migrate` calls (replicas starting together) are safe,
with per-engine behavior:

- **Postgres** holds an advisory lock on a dedicated connection for
  the whole call; concurrent runs fully serialize. A crash releases
  the lock with the connection.
- **SQLite** locks per migration (`BEGIN IMMEDIATE`); the final state
  is correct, but a losing runner can fail on a raced migration body
  and needs a retry.

## Testing against Postgres

The default test suite is hermetic (SQLite, in a nested `dbtest`
module so the library itself stays dependency-free). Postgres
integration tests cover apply/rerun/drift/orphan, multi-statement
bodies, and advisory-lock serialization:

```sh
TEST_POSTGRES_DSN='postgres://user:pass@localhost:5432/testdb?sslmode=disable' go test ./dbtest
```

## Errors

Branchable failures wrap sentinels: `ErrDrift`, `ErrOrphan`,
`ErrInvalidFilename`, `ErrDuplicateVersion`, `ErrDuplicateModule`,
`ErrInvalidSource`. I/O and SQL failures pass through wrapped with
the migration they belong to.
