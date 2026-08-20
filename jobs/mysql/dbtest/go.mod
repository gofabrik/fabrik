module github.com/gofabrik/fabrik/jobs/mysql/dbtest

go 1.27

require (
	github.com/go-sql-driver/mysql v1.10.0
	github.com/gofabrik/fabrik/jobs v0.1.0
)

require (
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/robfig/cron/v3 v3.0.1 // indirect
)

replace github.com/gofabrik/fabrik/jobs => ../..
