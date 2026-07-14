package shared

import "embed"

//fabrik:assets
//go:embed all:assets
var Assets embed.FS

//fabrik:migrations
//go:embed all:migrations
var Migrations embed.FS

//fabrik:templates
//go:embed all:templates
var Templates embed.FS
