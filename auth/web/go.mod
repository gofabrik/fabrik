module github.com/gofabrik/fabrik/auth/web

go 1.26

require (
	github.com/gofabrik/fabrik/auth v0.0.0
	github.com/gofabrik/fabrik/auth/password v0.0.0
)

require golang.org/x/crypto v0.43.0 // indirect

replace github.com/gofabrik/fabrik/auth => ../

replace github.com/gofabrik/fabrik/auth/password => ../password
