package httpapi

import (
	"encoding/json"
	"net/http"
)

// tunnelHandler records the current Cloudflare tunnel URL (POST, called by
// scripts/linux/build.sh once cloudflared prints it) or returns it (GET, used by
// the dashboard).
func (s *Server) tunnelHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var req struct {
			URL string `json:"url"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		s.Tunnel.Set(req.URL)

		jsonResponse(w, map[string]string{"status": "recorded"})

	case http.MethodGet:
		jsonResponse(w, map[string]string{"url": s.Tunnel.Get()})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
