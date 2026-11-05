package httpapi

import (
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/wsconn"
)

// A desktop's screen stream (the WebSocket noVNC opens at /desktop/<id>/websockify) is bridged by the control plane
// itself to the desktop's Xvnc (5901): RFB as it is, in binary WebSocket messages, both ways (Phase 2 C). It used to
// be reverse-proxied to websockify inside the desktop, a Python process (25-37 MB) per desktop doing only this. The
// path, the listeners it is on and their guards (origin, device sessions, a desktop never reaching another's) are
// unchanged: only what answers behind the path is. DECURIX_VNC_BRIDGE=0 goes back to websockify, which desktops still
// start until the bridge has its live check (docs-notes-CP); its own page (/desktop/<id>/vnc.html) is still served by
// it.

// Keepalive and idle for a bridged screen: the browser answers each ping, so a screen someone has open never idles
// out, and one whose browser went away is found within the idle time.
const (
	screenPing = 30 * time.Second
	screenIdle = 2 * time.Minute
	// screenDial bounds reaching the desktop's Xvnc before the WebSocket is accepted.
	screenDial = 5 * time.Second
)

// screenBridgeOn reports whether screen streams go through the control plane's bridge: unless DECURIX_VNC_BRIDGE is
// 0, off, false or no.
func screenBridgeOn() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DECURIX_VNC_BRIDGE"))) {
	case "0", "off", "false", "no":
		return false
	}
	return true
}

// isScreenStream reports whether a /desktop/<id>/<rest> request is the screen's WebSocket.
func isScreenStream(rest string, isWebSocket bool) bool {
	return isWebSocket && strings.Trim(rest, "/") == "websockify"
}

// bridgeScreen serves a desktop's screen stream through the bridge. The desktop's Xvnc is reached before the
// WebSocket is accepted, so a screen that cannot be reached is an HTTP error noVNC reports, not a socket that dies.
// It reports whether it answered; false means the address could not be resolved and the caller says why.
func (s *Server) bridgeScreen(w http.ResponseWriter, r *http.Request, rawID string) (answered bool, err error) {
	addr, desktopID, err := s.Desktops.ScreenStreamAddress(r.Context(), rawID)
	if err != nil {
		return false, err
	}
	tcp, err := net.DialTimeout("tcp", addr, screenDial)
	if err != nil {
		s.Desktops.InvalidateProxyTarget(desktopID)
		log.Printf("screen stream for %s: %v", desktopID, err)
		http.Error(w, "desktop unavailable", http.StatusBadGateway)
		return true, nil
	}
	c, err := wsconn.UpgradeGuarded(w, r)
	if err != nil {
		tcp.Close()
		return true, nil
	}
	done := make(chan struct{})
	defer close(done)
	go s.keepScreenViewed(desktopID, done)
	if err := wsconn.Bridge(c, tcp, screenPing, screenIdle); err != nil {
		log.Printf("screen stream for %s ended: %v", desktopID, err)
	}
	return true, nil
}
