package httpapi

import (
	"context"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/wsconn"
)

// A desktop's screen stream (the WebSocket noVNC opens at /desktop/<id>/websockify) is bridged by the control plane
// itself to the desktop's Xvnc (5901): RFB as it is, in binary WebSocket messages, both ways (Phase 2 C). It used to
// be reverse-proxied to websockify inside the desktop, a Python process (25-37 MB) per desktop doing only this. The
// path, the listeners it is on and their guards (origin, device sessions, a desktop never reaching another's) are
// unchanged: only what answers behind the path is. Desktops no longer start websockify (L3, docs/lightweight.md: the
// entrypoint passes the base startup --skip-novnc), so noVNC's own page (/desktop/<id>/vnc.html) answers only on a
// desktop created with DECURIX_DESKTOP_NOVNC=1. DECURIX_VNC_BRIDGE=0 still sends the stream to a desktop's websockify
// when one answers on 6901; a desktop that runs none is bridged all the same and the log says so (screenStreamBridged),
// so turning the bridge off never leaves a screen that cannot connect.

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

// screenStreamBridged reports whether desktop rawID's screen stream goes through the bridge: always, unless
// DECURIX_VNC_BRIDGE is off; then only when the desktop has no websockify answering (websockifyMissing).
func (s *Server) screenStreamBridged(ctx context.Context, rawID string) bool {
	if screenBridgeOn() {
		return true
	}
	return websockifyMissing(ctx, s.Desktops.ProxyTarget, rawID)
}

// websockifyMissing reports whether desktop rawID resolves but nothing answers on its websockify port (6901), and logs
// that its stream is bridged anyway. A desktop that does not resolve is not "missing": the reverse proxy reports why,
// as it did before.
func websockifyMissing(ctx context.Context, target func(context.Context, string) (*url.URL, string, error), rawID string) bool {
	u, id, err := target(ctx, rawID)
	if err != nil || u == nil {
		return false
	}
	c, err := net.DialTimeout("tcp", u.Host, time.Second)
	if err == nil {
		c.Close()
		return false
	}
	log.Printf("screen stream for %s: DECURIX_VNC_BRIDGE is off but the desktop runs no websockify (created without DECURIX_DESKTOP_NOVNC=1); bridging it", id)
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
