# query

Typed reads and struct-derived writes over `database/sql`. It gives
you generic scanning, simple write helpers, scoped transactions, and
constraint error classification for SQLite and Postgres. The core
module has no third-party dependencies.

## Reads

`All` and `One` run your SQL verbatim - write it in your driver's
placeholder style:

```go
// SQLite
todos, err := query.All[Todo](ctx, db, "SELECT * FROM todos WHERE done = ?", false)

// Postgres
user, err := query.One[User](ctx, db, "SELECT * FROM users WHERE id = $1", id)
```

Results are always structs. A single scalar uses a one-field struct.
Columns map to snake_case field names (`UserID` to `user_id`), with
`db:"name"` overrides and `db:"-"` skips. Unmatched result columns are
discarded. Duplicate result column names return an aliasing error.
`One` scans the first row and returns `ErrNotFound` on none; express
single-row intent with `LIMIT 1`.

Verbatim applies to SQL text only. Bind args are still normalized,
notably `time.Time` to RFC3339Nano text.

## The DB handle

`query.New` binds an executor and a dialect into a `*DB`. A `*DB` is
itself an `Executor`, so reads and writes share one value.

```go
q, err := query.New(db, query.DialectSQLite) // errors on a nil exec or unknown dialect
if err != nil {
	return err
}
id, err := q.Insert(ctx, "users", u)
users, err := query.All[User](ctx, q, "SELECT * FROM users")
```

`q.Tx(ctx, func(tx *query.DB) error { ... })` runs a transaction and hands
the callback a `*DB` bound to the `*sql.Tx` with the same dialect, so writes
stay dialect-free inside the transaction as well.

The free functions remain available when you want to pass the executor and,
for writes, the `Dialect` explicitly.

## Writes

The write helpers generate SQL, so as free functions they take a `Dialect`
(the handle binds it for you - see above):

```go
id, err := query.Insert(ctx, db, d, "users", u)
err = query.InsertMany(ctx, db, d, "events", batch)
n, err := query.Update(ctx, db, d, "users", "id = ?", patch, userID)
n, err = query.Delete(ctx, db, d, "sessions", "expires < ?", now)
```

Generated statements use `?`; on Postgres the whole statement is
rebound to `$1`, `$2`, and so on. The `where` fragment is included,
so always write `?` in `where`.

The rebinder is quote-aware, but it is not a SQL lexer. Empty `where`
fragments, unquoted `$1`, comments, dollar-quoted strings, Postgres
`E''` strings, unterminated quotes, numbered `?1` placeholders, and
detectable JSONB `?` operators are rejected. Queries needing those
features belong in raw SQL.

`Insert` is a convenience for the auto-PK convention: zero `id` is
omitted, and the return is `LastInsertId`. On Postgres and for UUID
keys, use `RETURNING`:

```go
type Returned struct{ ID int64 }
row, err := query.One[Returned](ctx, db,
	"INSERT INTO users (email) VALUES ($1) RETURNING id", email)
```

The `where` expression is trusted SQL by contract: bind every value
through a placeholder and never build it from user input. Table
names and derived column names are validated as identifiers
(`ErrInvalidIdentifier`) because identifiers can never be bound.

## Errors

Constraint failures classify to sentinels with no driver imports:

```go
if _, err := query.Insert(ctx, db, d, "users", u); errors.Is(err, query.ErrUnique) {
	return web.View(SignupPage{Error: "email already registered"}), nil
}
```

`ErrUnique`, `ErrForeignKey`, and `ErrCheck` cover SQLite (modernc,
mattn) and Postgres (pgx, lib/pq). `ErrNotFound` comes from `One`.
Use `RegisterClassifier` for other engines.

## Transactions

```go
err := query.Tx(ctx, db, func(tx *sql.Tx) error {
	if _, err := query.Insert(ctx, tx, d, "users", u); err != nil {
		return err
	}
	return query.InsertMany(ctx, tx, d, "audit", events)
})
```

`Tx` commits on nil, rolls back on error, and rolls back before
re-panicking. Every helper accepts `*sql.DB` and `*sql.Tx` through the
two-method `Executor` interface.

## Columns beyond scalars

`time.Time` is stored as RFC3339Nano text. Nullable time is
`*time.Time`: NULL maps to nil, values allocate. Use `sql.Null[T]`
for other nullable scalars and `query.JSON[T]` for TEXT, JSON, and
JSONB columns.

Structs are flat. Nested struct fields must be real column types:
Valuer for writes, Scanner for reads.

## Testing

The core suite is hermetic. Driver-backed tests live in the nested
`dbtest` module. SQLite runs by default; Postgres runs when
`TEST_POSTGRES_DSN` is set:

```sh
TEST_POSTGRES_DSN='postgres://user:pass@localhost:5432/testdb?sslmode=disable' go test ./dbtest
```
