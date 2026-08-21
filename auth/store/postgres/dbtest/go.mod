module github.com/gofabrik/fabrik/auth/store/postgres/dbtest

go 1.27

require (
	github.com/gofabrik/fabrik/auth/password v0.1.0
	github.com/gofabrik/fabrik/auth/store v0.1.0
	github.com/jackc/pgx/v5 v5.10.0
)

require (
	github.com/gofabrik/fabrik/auth v0.1.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/crypto v0.40.0 // indirect
	golang.org/x/sync v0.17.0 // indirect
	golang.org/x/sys v0.34.0 // indirect
	golang.org/x/text v0.29.0 // indirect
)

replace github.com/gofabrik/fabrik/auth/store => ../..

replace github.com/gofabrik/fabrik/auth => ../../..

replace github.com/gofabrik/fabrik/auth/password => ../../../password
