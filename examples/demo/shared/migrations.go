package shared

import "embed"

//fabrik:migrations module=shared
//go:embed all:migrations
var Migrations embed.FS
