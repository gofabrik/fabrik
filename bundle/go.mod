module github.com/gofabrik/fabrik/bundle

go 1.26

require (
	github.com/gofabrik/fabrik/assetmapper v0.0.0
	github.com/gofabrik/fabrik/migrations v0.0.0
	github.com/gofabrik/fabrik/templates v0.0.0
)

replace github.com/gofabrik/fabrik/assetmapper => ../assetmapper

replace github.com/gofabrik/fabrik/migrations => ../migrations

replace github.com/gofabrik/fabrik/templates => ../templates
