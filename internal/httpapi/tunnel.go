package httpapi

import (
	"encoding/json"
	"net/http"
)

// tunnelHandler records the addresses scripts/*/build.sh (or rebuild.sh)
// discovers for this machine (POST, one call for the admin/LAN URLs once
// Flowera is up, and one call per line each time cloudflared prints a new
// tunnel URL) or returns all of them (GET, used by the dashboard's Mobile
// menu). A field left out of a POST body leaves that address unchanged -
// the admin/LAN URLs and the tunnel URL are reported by separate calls, and
// neither should be able to blank out the other.
func (s *Server) tunnelHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var req struct {
			URL      string `json:"url"`
			AdminURL string `json:"admin_url"`
			LANURL   string `json:"lan_url"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if req.URL != "" {
			s.Tunnel.Set(req.URL)
		}

		if req.AdminURL != "" {
			s.Tunnel.SetAdminURL(req.AdminURL)
		}

		if req.LANURL != "" {
			s.Tunnel.SetLANURL(req.LANURL)
		}

		jsonResponse(w, map[string]string{"status": "recorded"})

	case http.MethodGet:
		jsonResponse(w, map[string]string{
			"url":       s.Tunnel.Get(),
			"admin_url": s.Tunnel.AdminURL(),
			"lan_url":   s.Tunnel.LANURL(),
		})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
