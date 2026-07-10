module github.com/gofabrik/fabrik/fabrik

go 1.26

require (
	github.com/gofabrik/fabrik/assetmapper v0.0.0
	github.com/gofabrik/fabrik/assetmapper/directive v0.0.0
	github.com/gofabrik/fabrik/config/directive v0.0.0
	github.com/gofabrik/fabrik/diag v0.0.0
	github.com/gofabrik/fabrik/gen v0.0.0
	github.com/gofabrik/fabrik/migrations/directive v0.0.0
	github.com/gofabrik/fabrik/router/directive v0.0.0
	github.com/gofabrik/fabrik/templates/directive v0.0.0
	github.com/gofabrik/fabrik/web/directive v0.0.0
	golang.org/x/tools v0.47.0
)

require (
	github.com/gofabrik/fabrik/migrations v0.0.0 // indirect
	github.com/gofabrik/fabrik/templates v0.0.0 // indirect
	golang.org/x/mod v0.37.0 // indirect
	golang.org/x/sync v0.21.0 // indirect
)

replace (
	github.com/gofabrik/fabrik/assetmapper => ../assetmapper
	github.com/gofabrik/fabrik/assetmapper/directive => ../assetmapper/directive
	github.com/gofabrik/fabrik/config/directive => ../config/directive
	github.com/gofabrik/fabrik/diag => ../diag
	github.com/gofabrik/fabrik/gen => ../gen
	github.com/gofabrik/fabrik/migrations => ../migrations
	github.com/gofabrik/fabrik/migrations/directive => ../migrations/directive
	github.com/gofabrik/fabrik/router/directive => ../router/directive
	github.com/gofabrik/fabrik/templates => ../templates
	github.com/gofabrik/fabrik/templates/directive => ../templates/directive
	github.com/gofabrik/fabrik/web => ../web
	github.com/gofabrik/fabrik/web/directive => ../web/directive
)
