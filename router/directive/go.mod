module github.com/gofabrik/fabrik/router/directive

go 1.26

require (
	github.com/gofabrik/fabrik/diag v0.0.0
	github.com/gofabrik/fabrik/gen v0.0.0
)

require (
	golang.org/x/mod v0.26.0 // indirect
	golang.org/x/sync v0.16.0 // indirect
	golang.org/x/tools v0.35.0 // indirect
)

replace (
	github.com/gofabrik/fabrik/diag => ../../diag
	github.com/gofabrik/fabrik/gen => ../../gen
)
