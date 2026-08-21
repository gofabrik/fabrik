module github.com/gofabrik/fabrik/auth/store/mysql/dbtest

go 1.27

require (
	github.com/go-sql-driver/mysql v1.10.0
	github.com/gofabrik/fabrik/auth/password v0.1.0
	github.com/gofabrik/fabrik/auth/store v0.1.0
)

require (
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/gofabrik/fabrik/auth v0.1.0 // indirect
	golang.org/x/crypto v0.40.0 // indirect
	golang.org/x/sys v0.34.0 // indirect
)

replace github.com/gofabrik/fabrik/auth/store => ../..

replace github.com/gofabrik/fabrik/auth => ../../..

replace github.com/gofabrik/fabrik/auth/password => ../../../password
