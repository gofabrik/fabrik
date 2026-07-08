package web

import "embed"

//fabrik:http:static /assets dir=assets
//go:embed assets
var Assets embed.FS
