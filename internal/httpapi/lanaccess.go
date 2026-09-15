package httpapi

import "net/http"

// lanAccessStatusHandler returns the current LAN/tunnel access code, for the
// dashboard's Mobile menu to show next to the tunnel URL. Registered only on
// the primary mux (see Server.RegisterLocalRoutes) - never served back over
// the listener it gates.
func (s *Server) lanAccessStatusHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.LANAccess == nil {
		jsonResponse(w, map[string]any{"enabled": false})
		return
	}

	jsonResponse(w, map[string]any{
		"enabled": true,
		"code":    s.LANAccess.Password(),
	})
}

// lanAccessRotateHandler replaces the LAN/tunnel access code and drops every
// session it had granted - the "kick everyone off" button next to it in the
// dashboard's Mobile menu.
func (s *Server) lanAccessRotateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.LANAccess == nil {
		jsonStatusError(w, http.StatusNotFound, "LAN access is not configured")
		return
	}

	jsonResponse(w, map[string]any{"code": s.LANAccess.Rotate()})
}
