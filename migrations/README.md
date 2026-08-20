# migrations

Forward-only SQL migrations for `database/sql`: plain `NNNN_name.sql`
files, checksummed against drift, applied in version order. SQLite,
Postgres, and MySQL/MariaDB via per-database driver packages.

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
if err := migrations.Migrate(ctx, db, sqlite.Driver(), fsys); err != nil {
	log.Fatal(err)
}
```

The backend is a `Driver` value from the `sqlite`, `postgres`, or
`mysql` subpackage (`mysql` serves both MySQL and MariaDB). The
`Driver` interface is exported so other backends can implement it; its
shape is unstable before 1.0.0.

In a fabrik application, config-driven backend selection can use a
`//fabrik:provider:select` group whose cases return the leaf drivers.

`Migrate` is idempotent; each migration commits in its own
transaction, and a failure keeps everything already applied
(re-running resumes). A file may contain several statements; the
whole body runs inside the migration's transaction. `Status` reports
every migration as `pending`, `applied`, `drifted`, or `orphan`.

**MySQL/MariaDB exception**: both servers implicitly commit DDL, so a
failed migration can leave earlier statements applied without a
`schema_migrations` record. Use single-statement or idempotent migrations.
Two DSN options are required:
`multiStatements=true` for multi-statement bodies and `parseTime=true`
for `Status` timestamps. The `schema_migrations` key columns are
`VARBINARY(191)`; stream and file names beyond 191 bytes are rejected.

## Streams

Independent packages cannot coordinate a global version sequence, so
migrations group into **streams**: versions order within a stream,
streams are independent, and bookkeeping is keyed by
`(stream, version)`:

```go
srcs := migrations.Sources{
	{Stream: "auth", FS: auth.Migrations, Dir: "migrations"},
	{Stream: "todos", FS: todos.Migrations, Dir: "migrations"},
}
if err := srcs.Migrate(ctx, db, sqlite.Driver()); err != nil { ... }
```

Streams apply in sorted stream order, one engine session and one lock
across the whole call. A failing migration skips the rest of its
stream and all later streams; applied work stays.

Tables that reference each other belong in one stream. Streams compose
freely because there is no cross-stream dependency ordering.

A `Migrate` or `Status` call owns the whole `schema_migrations`
table: applied rows in streams not present among the sources are
orphans. Removing a package's migrations without cleaning its rows is
loud, never silent. This library assumes it owns `schema_migrations`;
sharing that table with another migration tool is out of scope.

## Concurrency

Concurrent `Migrate` calls (replicas starting together) are safe,
with per-engine behavior:

- **Postgres** holds an advisory lock on a dedicated connection for
  the whole call; concurrent runs fully serialize. A crash releases
  the lock with the connection.
- **SQLite** locks per migration (`BEGIN IMMEDIATE`); the final state
  is correct, but a losing runner can fail on a raced migration body
  and needs a retry.
- **MySQL/MariaDB** hold a `GET_LOCK` named lock on a dedicated
  connection for the whole call; concurrent runs fully serialize. A
  crash releases the lock with the connection.

## Testing against live databases

The default test suite is hermetic. Live integration tests run on every
backend whose DSN is set. `task db:up` at the repository root starts all
three databases:

```sh
TEST_POSTGRES_DSN=... TEST_MYSQL_DSN=... TEST_MARIADB_DSN=... go test ./dbtest
```

## Errors

Branchable failures wrap sentinels: `ErrDrift`, `ErrOrphan`,
`ErrInvalidFilename`, `ErrDuplicateVersion`, `ErrDuplicateStream`,
`ErrInvalidSource`. I/O and SQL failures pass through wrapped with
the migration they belong to.
