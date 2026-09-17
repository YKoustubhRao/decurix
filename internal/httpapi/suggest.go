package httpapi

import (
	"context"
	"net/http"
	"time"
)

// suggestTimeout bounds how long the requests page's compose form waits for
// a desktop suggestion. A cold or unreachable Ollama must never hang the
// request - the frontend treats a slow/failed suggestion the same as no
// suggestion at all.
const suggestTimeout = 8 * time.Second

// suggestDesktopHandler recommends a desktop for a request's content, or an
// empty object when there is nothing to suggest. This is decoration for the
// requests page's compose form: a failure here is never an error, only an
// empty suggestion.
func (s *Server) suggestDesktopHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), suggestTimeout)
	defer cancel()

	suggestion, ok := s.Suggest.Suggest(ctx, r.FormValue("content"))
	if !ok {
		jsonResponse(w, map[string]any{})
		return
	}

	jsonResponse(w, suggestion)
}
