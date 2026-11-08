package httpapi

import (
	"log"
	"net/http"

	"github.com/YKoustubhRao/decurix/internal/desktop"
)

// pageDesktops lists the desktops a page shows. When they cannot be listed (Docker is down or still starting, as on
// the Windows app while its distro boots), the page still renders with none: its Docker banner says why, and it
// notices by itself when Docker is back. A page answering "internal server error" instead hid that banner (found by
// the 2026-11-07 smoke test of a control plane with no Docker).
func (s *Server) pageDesktops(r *http.Request, page string) []desktop.Desktop {
	desktops, err := s.Desktops.List(r.Context())
	if err != nil {
		log.Printf("%s: listing desktops: %v (rendering with none)", page, err)
		return nil
	}
	return desktops
}
