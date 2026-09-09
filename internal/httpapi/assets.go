package httpapi

import (
	"net/http"

	"github.com/YKoustubhRao/flowera/internal/webui"
)

// faviconHandler serves the embedded Flowera mark as the site favicon, which
// every page points at with <link rel="icon"> in the shared "theme" template.
func (s *Server) faviconHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(webui.FaviconSVG)
}
