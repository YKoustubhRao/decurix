package httpapi

import (
	"net/http"
	"strings"
)

// nativeScreenHandler serves a native Windows worker's screen. It is the
// viewer every desktop has (screen.go, web/templates/screen.html), over a VNC
// endpoint of the control plane's own in place of the one a desktop container
// runs (nativerfb.go):
//
//	/desktop/<id>/              the viewer; it opens for a stopped worker too,
//	                            and starts it
//	/desktop/<id>/websockify    the viewer's WebSocket: the worker's private
//	                            desktop, spoken as VNC
//	/desktop/<id>/input-state   where its keys would go and what is on it, in
//	                            the helper's own words: nobody can look at a
//	                            private desktop to find out
func (s *Server) nativeScreenHandler(w http.ResponseWriter, r *http.Request, rawID, rest string) {
	isWebSocket := strings.EqualFold(r.Header.Get("Upgrade"), "websocket")

	switch {
	case rest == "websockify" && isWebSocket:
		s.nativeRFBHandler(w, r, rawID)

	case rest == "input-state" && r.Method == http.MethodGet && !isWebSocket:
		sess, err := s.Desktops.NativeSession(rawID)
		if err != nil {
			http.Error(w, "the worker is not running", http.StatusServiceUnavailable)
			return
		}
		focus, _ := sess.Do("focus")
		windows, _ := sess.Do("windows")
		jsonResponse(w, map[string]string{"keys_go_to": focus, "windows": windows})

	case rest == "" && !isWebSocket && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		s.screenViewerHandler(w, r, rawID)

	default:
		http.NotFound(w, r)
	}
}
