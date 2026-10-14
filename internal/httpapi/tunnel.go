package httpapi

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// maxTunnelRequestBytes bounds one POST /api/tunnel body - four short URLs,
// never legitimately more than a few hundred bytes.
const maxTunnelRequestBytes = 8192

// tunnelHandler records the addresses scripts/*/build.sh (or rebuild.sh)
// discovers for this machine (POST, one call for the admin/LAN URLs once
// Decurix is up, and one call per line each time either Cloudflare tunnel -
// the unauthenticated one at :8080 or the access-code-gated one - prints a
// new URL) or returns all of them (GET, used by the dashboard's Mobile
// menu). A field left out of a POST body leaves that address unchanged -
// the admin/LAN URLs and the two tunnel URLs are reported by separate calls,
// and none of them should be able to blank out another. An explicit empty
// lan_url or lan_tunnel_url clears an unavailable link from the Mobile menu.
func (s *Server) tunnelHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var req struct {
			URL          string  `json:"url"`
			AdminURL     string  `json:"admin_url"`
			LANURL       *string `json:"lan_url"`
			LANTunnelURL *string `json:"lan_tunnel_url"`
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxTunnelRequestBytes)
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

		if req.LANURL != nil {
			s.Tunnel.SetLANURL(*req.LANURL)
		}

		if req.LANTunnelURL != nil && (*req.LANTunnelURL == "" || tunnelAddress(*req.LANTunnelURL)) {
			s.Tunnel.SetLANTunnelURL(*req.LANTunnelURL)
		}

		jsonResponse(w, map[string]string{"status": "recorded"})

	case http.MethodGet:
		// A named tunnel's fixed link replaces whatever quick tunnel a
		// script reported: it is the one that survives a restart.
		lanTunnelURL := s.Tunnel.LANTunnelURL()
		if !tunnelAddress(lanTunnelURL) {
			lanTunnelURL = "" // stored by a script from before the check
		}
		if s.CFTunnel != nil {
			if named := s.CFTunnel.URL(); named != "" {
				lanTunnelURL = named
			}
		}

		jsonResponse(w, map[string]string{
			"url":            s.Tunnel.Get(),
			"admin_url":      s.Tunnel.AdminURL(),
			"lan_url":        s.Tunnel.LANURL(),
			"lan_tunnel_url": lanTunnelURL,
		})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// tunnelAddress reports whether raw can be a tunnel's own address. Cloudflare's
// API host is not one: cloudflared names it in the error it logs when a quick
// tunnel cannot be created, and a script that took it for the tunnel handed
// out links that answered "method not allowed".
func tunnelAddress(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}

	host := strings.ToLower(u.Hostname())

	return host != "trycloudflare.com" && host != "api.trycloudflare.com"
}
