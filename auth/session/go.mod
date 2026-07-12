module github.com/gofabrik/fabrik/auth/session

go 1.26

require (
	github.com/gofabrik/fabrik/auth v0.0.0
	github.com/gofabrik/fabrik/session v0.0.0
)

replace github.com/gofabrik/fabrik/auth => ..

replace github.com/gofabrik/fabrik/session => ../../session
