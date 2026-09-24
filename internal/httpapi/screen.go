package httpapi

import (
	"bytes"
	"errors"
	"html/template"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/webui"
)

// screenViewedEvery is how often an open screen tells idle-pause it is in use.
// The WebSocket is one request that lasts as long as the screen is open, so
// the touch resolveProxyTarget makes when it connects was the only one: a
// person watching or typing for five minutes had their desktop paused under
// them once nothing else (the recording toolbar's polling) kept it awake.
// Well inside idle-pause's shortest window - the fast pause 15 s after a task
// ends (idlepause.FastPauseAfter) - so a screen someone has open is never
// frozen under them; measured, a 30 s tick let it pause mid-session.
var screenViewedEvery = 5 * time.Second

// screenViewerHandler serves a desktop's screen (/desktop/<id>/): the viewer
// in web/templates/screen.html, rendered by the control plane itself rather
// than proxied from the desktop, so it opens (and can start the desktop) even
// when the desktop is stopped, and loads one cached file instead of noVNC's
// page file by file from a 1-vCPU container.
func (s *Server) screenViewerHandler(w http.ResponseWriter, r *http.Request, rawID string) {
	access, err := s.Desktops.ScreenAccess(r.Context(), rawID)
	if errors.Is(err, desktop.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		log.Printf("screen for %s: %v", rawID, err)
		http.Error(w, "the desktop's screen could not be opened", http.StatusInternalServerError)
		return
	}

	controls := strings.ReplaceAll(ticketToolbar, "__DESKTOP_ID__", access.ID) +
		strings.ReplaceAll(resourceAlertPopup, "__DESKTOP_ID__", access.ID)

	var out bytes.Buffer
	if err := s.Web.RenderScreen(&out, webui.ScreenPageData{
		ID:       access.ID,
		Name:     access.Name,
		Password: access.Password,
		NoVNC:    noVNCPath,
		// Server-owned markup (proxy.go), with only a sanitized ID spliced in.
		Controls: template.HTML(controls),
	}); err != nil {
		jsonError(w, err)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The page carries the desktop's screen password: never cached, never
	// sent on as a referrer.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Write(out.Bytes())
}

// keepScreenViewed marks a desktop viewed every screenViewedEvery until done
// is closed - for as long as its screen's WebSocket is open.
func (s *Server) keepScreenViewed(desktopID string, done <-chan struct{}) {
	ticker := time.NewTicker(screenViewedEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.Desktops.MarkViewed(desktopID)
		case <-done:
			return
		}
	}
}
