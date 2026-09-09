// Package web embeds the control plane's HTML assets into the binary so a
// compiled flowera executable can be copied to another machine and run on its
// own, with no repo checkout sitting next to it.
package web

import "embed"

// TemplatesFS holds web/templates/*.html. Paths inside it always use forward
// slashes (io/fs convention), so join them with path.Join, never filepath.Join.
//
//go:embed templates/*.html
var TemplatesFS embed.FS
