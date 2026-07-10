module demo

go 1.26

require (
	github.com/gofabrik/fabrik/assetmapper v0.0.0
	github.com/gofabrik/fabrik/config v0.0.0
	github.com/gofabrik/fabrik/router v0.0.0
	github.com/gofabrik/fabrik/templates v0.0.0
	github.com/gofabrik/fabrik/web v0.0.0
)

require gopkg.in/yaml.v3 v3.0.1 // indirect

replace (
	github.com/gofabrik/fabrik/assetmapper => ../../assetmapper
	github.com/gofabrik/fabrik/config => ../../config
	github.com/gofabrik/fabrik/router => ../../router
	github.com/gofabrik/fabrik/templates => ../../templates
	github.com/gofabrik/fabrik/web => ../../web
)
