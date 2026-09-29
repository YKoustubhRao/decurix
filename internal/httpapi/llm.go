package httpapi

import (
	"errors"
	"io"
	"log"
	"net/http"

	"github.com/YKoustubhRao/decurix/internal/llm"
)

// llmStatusHandler says whether a hosted model is configured, and which -
// what the create dialog reads to offer the "azure" harness. Never the key.
func (s *Server) llmStatusHandler(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"configured": s.LLM.Configured()}

	if s.LLM.Configured() {
		out["model"] = s.LLM.Model
		out["fast_model"] = s.LLM.FastModel
	}

	jsonResponse(w, out)
}

// llmChatHandler forwards one chat request from a desktop's agent to the
// hosted model (internal/llm), adding the key the desktop never sees. It is
// registered on the primary listener only (RegisterLocalRoutes): the agents
// reach it over the Docker network, and nobody holding the gated listener's
// access code gets a free model to spend on.
func (s *Server) llmChatHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, llm.MaxRequestBytes+1))
	if err != nil {
		http.Error(w, "could not read the request", http.StatusBadRequest)
		return
	}

	if len(body) > llm.MaxRequestBytes {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}

	status, answer, retry, err := s.LLM.Chat(r.Context(), body)
	if err != nil {
		if errors.Is(err, llm.ErrNotConfigured) {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}

		log.Printf("llm: %v", err)
		http.Error(w, "the hosted model could not be reached", http.StatusBadGateway)
		return
	}

	// A rate limit's "try again in N seconds" goes through to the desktop.
	for name, values := range retry {
		for _, v := range values {
			w.Header().Add(name, v)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(answer)
}
