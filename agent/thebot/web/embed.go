// Package web embeds the in-VM chat UI directly into the soul binary -
// one artifact to ship, no separate static-file directory to keep in
// sync with the Docker image. Served at "/" by cmd/soul.
package web

import "embed"

//go:embed index.html
var FS embed.FS
