package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/YKoustubhRao/decurix/internal/cftunnel"
)

// cfTunnelSetupTimeout bounds one setup: a handful of Cloudflare API calls.
const cfTunnelSetupTimeout = 45 * time.Second

// cfTunnelStatusHandler reports the named Cloudflare tunnel (the fixed link
// on the person's own domain) and this machine's connector for it, for the
// dashboard's Mobile menu and for build.sh, which skips its quick tunnel
// while one is set up. Primary listener only, and never to a desktop.
func (s *Server) cfTunnelStatusHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.CFTunnel == nil {
		jsonResponse(w, cftunnel.Status{Connections: -1})
		return
	}

	jsonResponse(w, s.CFTunnel.Status(r.Context()))
}

// cfTunnelConfigureHandler sets the named tunnel up from the form values
// api_token, domain and subdomain (optional, "decurix"). An empty api_token
// reuses the saved one. Cloudflare's own refusal is shown as it is - it is
// the person's own token, and its message is what tells them what to fix.
func (s *Server) cfTunnelConfigureHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.CFTunnel == nil {
		jsonStatusError(w, http.StatusNotFound, "the named tunnel is not available")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), cfTunnelSetupTimeout)
	defer cancel()

	status, err := s.CFTunnel.Configure(ctx, r.FormValue("api_token"), r.FormValue("domain"), r.FormValue("subdomain"))

	var apiErr *cftunnel.APIError

	switch {
	case err == nil:
		jsonResponse(w, status)
	case errors.Is(err, cftunnel.ErrInvalid):
		jsonStatusError(w, http.StatusBadRequest, "Enter an API token, a domain (example.com) and a name made of letters, digits and dashes.")
	case errors.Is(err, cftunnel.ErrZoneNotFound):
		jsonStatusError(w, http.StatusBadRequest, "That domain isn't on the Cloudflare account this token belongs to, or the token can't read it (it needs Zone Read).")
	case errors.Is(err, cftunnel.ErrNameTaken):
		jsonStatusError(w, http.StatusConflict, "That name already has a DNS record pointing somewhere else. Pick another name, or delete the record in Cloudflare.")
	case errors.As(err, &apiErr):
		jsonStatusError(w, http.StatusBadGateway, "Cloudflare said: "+apiErr.Message+". The token needs Cloudflare Tunnel Edit, DNS Edit and Zone Read.")
	default:
		jsonError(w, err)
	}
}

// cfTunnelDisableHandler stops this machine's connector and forgets the
// setup; the tunnel and its DNS record stay on Cloudflare.
func (s *Server) cfTunnelDisableHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.CFTunnel == nil {
		jsonStatusError(w, http.StatusNotFound, "the named tunnel is not available")
		return
	}

	if err := s.CFTunnel.Disable(); err != nil {
		jsonError(w, err)
		return
	}

	jsonResponse(w, map[string]any{"configured": false})
}
