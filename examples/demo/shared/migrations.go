package shared

import "embed"

//fabrik:migrations
//go:embed all:migrations
var Migrations embed.FS
