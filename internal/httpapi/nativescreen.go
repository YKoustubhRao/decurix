package httpapi

import (
	"bytes"
	"errors"
	"html/template"
	"log"
	"net/http"
	"strings"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/webui"
)

// nativeScreenHandler serves a native Windows worker's screen, in place of
// the VNC viewer and proxy a desktop container has:
//
//	/desktop/<id>/         the page (web/templates/nativescreen.html); it
//	                       opens for a stopped worker too, and offers to start it
//	/desktop/<id>/screen   the page's WebSocket: the worker's private desktop
//	                       as pictures pushed when it changes, and the person's
//	                       mouse and keyboard back (serveSessionSocket)
//
// The worker's desktop is not on anybody's screen, so this is the only place
// a person sees it - and what they do here is done there.
func (s *Server) nativeScreenHandler(w http.ResponseWriter, r *http.Request, rawID, rest string) {
	isWebSocket := strings.EqualFold(r.Header.Get("Upgrade"), "websocket")

	if rest == "screen" && isWebSocket {
		sess, err := s.Desktops.NativeSession(rawID)
		switch {
		case errors.Is(err, desktop.ErrNotFound):
			http.NotFound(w, r)
			return
		case err != nil:
			http.Error(w, "the worker is not running", http.StatusServiceUnavailable)
			return
		}

		id := desktop.SanitizeID(rawID)
		s.serveSessionSocket(w, r, sess, "worker "+id, id, s.recordViewerInput(id))

		return
	}

	// /desktop/<id>/input-state: what is on the worker's desktop and where its keys would go now, in the helper's
	// own words. Nobody can look at a private desktop, so "typing does nothing" has no other way to be explained.
	if rest == "input-state" && r.Method == http.MethodGet && !isWebSocket {
		sess, err := s.Desktops.NativeSession(rawID)
		if err != nil {
			http.Error(w, "the worker is not running", http.StatusServiceUnavailable)
			return
		}
		focus, _ := sess.Do("focus")
		windows, _ := sess.Do("windows")
		jsonResponse(w, map[string]string{"keys_go_to": focus, "windows": windows})
		return
	}

	if rest != "" || isWebSocket || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		http.NotFound(w, r)
		return
	}

	access, err := s.Desktops.ScreenAccess(r.Context(), rawID)
	if errors.Is(err, desktop.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		log.Printf("screen for %s: %v", rawID, err)
		http.Error(w, "the worker's screen could not be opened", http.StatusInternalServerError)
		return
	}

	var out bytes.Buffer
	if err := s.Web.RenderNativeScreen(&out, webui.NativeScreenPageData{
		ID:   access.ID,
		Name: access.Name,
		// The recording toolbar every screen carries (proxy.go): server-owned
		// markup with only a sanitized ID spliced in.
		Controls: template.HTML(strings.ReplaceAll(ticketToolbar, "__DESKTOP_ID__", access.ID)),
	}); err != nil {
		jsonError(w, err)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(out.Bytes())
}
