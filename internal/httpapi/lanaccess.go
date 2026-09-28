package httpapi

import (
	"errors"
	"net/http"

	"github.com/YKoustubhRao/decurix/internal/devices"
)

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

// lanAccessDevicesHandler lists the devices paired with the gated listener,
// for the Mobile menu. Primary listener only, like the code itself: a paired
// phone should not be able to see, or revoke, the others.
func (s *Server) lanAccessDevicesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.Devices == nil {
		jsonResponse(w, map[string]any{"enabled": false, "devices": []devices.Device{}})
		return
	}

	list, err := s.Devices.List()
	if err != nil {
		jsonError(w, err)
		return
	}

	jsonResponse(w, map[string]any{"enabled": true, "devices": list})
}

// lanAccessRevokeDeviceHandler unpairs one device (form value id): the next
// request it makes meets the access-code gate again.
func (s *Server) lanAccessRevokeDeviceHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.Devices == nil {
		jsonStatusError(w, http.StatusNotFound, "device pairing is not configured")
		return
	}

	err := s.Devices.Revoke(r.FormValue("id"))
	if errors.Is(err, devices.ErrNotFound) {
		jsonStatusError(w, http.StatusNotFound, "no such device")
		return
	}
	if err != nil {
		jsonError(w, err)
		return
	}

	jsonResponse(w, map[string]any{"revoked": true})
}
