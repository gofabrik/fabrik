# Development

Running the example app with fabrik CLI (from the repository root):

go run ./fabrik run examples/demo migrate
go run ./fabrik run examples/demo run

## Database-backed tests

Some test modules require live Postgres, MySQL, and MariaDB instances.
They skip when their DSN environment variables are unset.

Start the databases (Docker required):

```
task db:up
```

Run the database-backed test modules:

```
task test:db
```

The local DSNs match CI and `task test:db`:

```
TEST_POSTGRES_DSN='postgres://postgres:proto@localhost:55432/proto?sslmode=disable'
TEST_MYSQL_DSN='root:proto@tcp(localhost:53306)/proto?parseTime=true&multiStatements=true'
TEST_MARIADB_DSN='root:proto@tcp(localhost:53307)/proto?parseTime=true&multiStatements=true'
```

Stop and remove the containers when done:

```
task db:down
```

`task ci` runs `task test` across all modules.
