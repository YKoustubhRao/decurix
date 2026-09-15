package httpapi

import (
	"encoding/json"
	"net/http"
)

// tunnelHandler records the addresses scripts/*/build.sh (or rebuild.sh)
// discovers for this machine (POST, one call for the admin/LAN URLs once
// Flowera is up, and one call per line each time either Cloudflare tunnel -
// the unauthenticated one at :8080 or the access-code-gated one - prints a
// new URL) or returns all of them (GET, used by the dashboard's Mobile
// menu). A field left out of a POST body leaves that address unchanged -
// the admin/LAN URLs and the two tunnel URLs are reported by separate calls,
// and none of them should be able to blank out another.
func (s *Server) tunnelHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var req struct {
			URL          string `json:"url"`
			AdminURL     string `json:"admin_url"`
			LANURL       string `json:"lan_url"`
			LANTunnelURL string `json:"lan_tunnel_url"`
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

		if req.LANTunnelURL != "" {
			s.Tunnel.SetLANTunnelURL(req.LANTunnelURL)
		}

		jsonResponse(w, map[string]string{"status": "recorded"})

	case http.MethodGet:
		jsonResponse(w, map[string]string{
			"url":            s.Tunnel.Get(),
			"admin_url":      s.Tunnel.AdminURL(),
			"lan_url":        s.Tunnel.LANURL(),
			"lan_tunnel_url": s.Tunnel.LANTunnelURL(),
		})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
