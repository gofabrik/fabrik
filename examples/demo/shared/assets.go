package shared

import "embed"

// Assets compile at startup: hashed URLs, references rewritten.
//
//fabrik:assets
//go:embed all:assets
var Assets embed.FS
