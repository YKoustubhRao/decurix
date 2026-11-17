// Package httpapi: screenholders.go keeps a desktop's screen to one window at a time.
package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
)

// A desktop's screen is open in one window at a time: a second window, on this computer or through a link, waits
// until the first is closed. A window is "holding" for as long as its screen stream (the WebSocket under
// /desktop/<id>/websockify) is open; a window that can no longer be reached (a laptop shut, a network gone) loses
// its stream when it stops answering the bridge's pings (screenIdle), and the screen is free again.
//
// Each screen page makes a holder token when it loads and sends it with its stream (?holder=). The same token again
// is the same window reconnecting: it takes the screen back and its older stream, a leftover the bridge has not yet
// found dead, is closed. A stream with no token (noVNC's own page, an old cached viewer) is a window of its own.

// screenHold is the window holding one desktop's screen.
type screenHold struct {
	holder string
	gen    uint64 // which take this is, so a stale release does not free a newer hold
	kick   func() // closes this hold's stream
	since  time.Time
}

// screenHolders is which window holds each desktop's screen. The zero value is ready to use.
type screenHolders struct {
	mu    sync.Mutex
	gen   uint64
	holds map[string]*screenHold
}

// take gives desktopID's screen to holder, whose stream kick closes. It refuses (ok false) while another window holds
// it. The same holder takes it back from its own older stream, which is kicked. release frees the screen unless a
// newer take has it.
func (h *screenHolders) take(desktopID, holder string, kick func()) (release func(), ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.holds == nil {
		h.holds = map[string]*screenHold{}
	}
	if current := h.holds[desktopID]; current != nil {
		if current.holder != holder {
			return nil, false
		}
		go current.kick()
	}
	h.gen++
	gen := h.gen
	h.holds[desktopID] = &screenHold{holder: holder, gen: gen, kick: kick, since: time.Now()}
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if current := h.holds[desktopID]; current != nil && current.gen == gen {
			delete(h.holds, desktopID)
		}
	}, true
}

// heldElsewhere reports whether desktopID's screen is held by a window other than holder, and since when.
func (h *screenHolders) heldElsewhere(desktopID, holder string) (bool, time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	current := h.holds[desktopID]
	if current == nil || current.holder == holder {
		return false, time.Time{}
	}
	return true, current.since
}

// screenHolderToken is the window token a request names (?holder=), or a fresh one of its own when it names none
// usable: at most 64 letters, digits, '-' or '_'.
func screenHolderToken(r *http.Request) string {
	token := r.URL.Query().Get("holder")
	if len(token) == 0 || len(token) > 64 {
		return newScreenHolderToken()
	}
	for _, c := range token {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return newScreenHolderToken()
		}
	}
	return token
}

// newScreenHolderToken is a token no window has: a stream that names none is a window of its own.
func newScreenHolderToken() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "anon-" + hex.EncodeToString(b[:])
}

// screenHolderHandler answers GET /api/desktops/screen-holder?id=<desktop>&holder=<token>: whether the desktop's
// screen is open in another window. The screen page asks before it connects, and waits while it is.
func (s *Server) screenHolderHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	id := desktop.SanitizeID(r.URL.Query().Get("id"))
	if id == "" {
		jsonStatusError(w, http.StatusBadRequest, "missing id")
		return
	}
	held, since := s.screens.heldElsewhere(id, r.URL.Query().Get("holder"))
	answer := map[string]any{"held": held}
	if held {
		answer["since"] = since.UTC().Format(time.RFC3339)
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, answer)
}

// refuseScreenInUse answers a stream that would open a screen held by another window: 409, before any upgrade.
func refuseScreenInUse(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, "this desktop's screen is open in another window", http.StatusConflict)
}
