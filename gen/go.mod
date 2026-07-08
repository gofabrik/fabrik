module github.com/gofabrik/fabrik/gen

go 1.26

require (
	github.com/gofabrik/fabrik/diag v0.0.0
	golang.org/x/tools v0.35.0
)

require (
	golang.org/x/mod v0.26.0 // indirect
	golang.org/x/sync v0.16.0 // indirect
)

replace github.com/gofabrik/fabrik/diag => ../diag
