// Package web embeds the dashboard templates and static assets.
package web

import "embed"

// FS holds templates/ and static/.
//
//go:embed templates static
var FS embed.FS
