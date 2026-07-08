// Package web embeds the built-in dashboard frontend.
package web

import "embed"

//go:embed static
var Static embed.FS
