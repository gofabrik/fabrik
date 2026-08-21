module github.com/gofabrik/fabrik/jobs/postgres/dbtest

go 1.27

require (
	github.com/gofabrik/fabrik/jobs v0.1.0
	github.com/jackc/pgx/v5 v5.10.0
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/robfig/cron/v3 v3.0.1 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/text v0.38.0 // indirect
)

replace github.com/gofabrik/fabrik/jobs => ../..
