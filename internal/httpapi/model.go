package httpapi

import (
	"net/http"

	"github.com/YKoustubhRao/decurix/internal/ollama"
)

// modelHandler serves GET /api/model: the chat model new desktops get, the
// model that fits this machine best right now, and the resources that
// decided it (what the top bar's model chip shows).
func (s *Server) modelHandler(w http.ResponseWriter, r *http.Request) {
	if !requireGet(w, r) {
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, ollama.CurrentStatus(r.Context()))
}
