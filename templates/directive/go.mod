module github.com/gofabrik/fabrik/templates/directive

go 1.26

require (
	github.com/gofabrik/fabrik/diag v0.0.0
	github.com/gofabrik/fabrik/gen v0.0.0
	github.com/gofabrik/fabrik/templates v0.0.0
)

require golang.org/x/tools v0.47.0 // indirect

replace (
	github.com/gofabrik/fabrik/diag => ../../diag
	github.com/gofabrik/fabrik/gen => ../../gen
	github.com/gofabrik/fabrik/templates => ..
)
