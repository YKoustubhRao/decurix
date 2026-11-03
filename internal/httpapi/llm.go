package httpapi

import (
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/llm"
)

// llmStatusHandler says which hosted models are set up, and with which model
// names - what the create dialog reads to offer them as a desktop's brain.
// Never a key.
func (s *Server) llmStatusHandler(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, map[string]any{
		"configured": s.LLM.AnyConfigured(),
		"providers":  s.LLM.Status(),
		"defaults": map[string]any{
			llm.ProviderClaude: map[string]string{"model": llm.DefaultClaudeModel, "fast_model": llm.DefaultClaudeFastModel},
			llm.ProviderAzure:  map[string]string{"model": llm.DefaultModel, "fast_model": llm.DefaultFastModel},
			llm.ProviderOpenAI: map[string]string{"model": llm.DefaultModel, "fast_model": llm.DefaultFastModel, "endpoint": llm.DefaultOpenAIEndpoint},
		},
	})
}

// llmSettingsHandler is where a person sets up a hosted model: GET is the
// status (as /api/llm), POST provider + key (+ endpoint for Azure, + model
// names) tries the settings with one short question and saves them only if
// it was answered. An empty key keeps the saved one, so changing a model name
// does not mean typing the key again. Primary listener and humansOnly: the key
// is the person's, never a desktop's and never the gated listener's.
func (s *Server) llmSettingsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.llmStatusHandler(w, r)
		return
	}

	if !allowMethod(w, r, http.MethodPost) {
		return
	}

	provider := strings.TrimSpace(r.FormValue("provider"))
	if !llm.ValidProvider(provider) {
		jsonStatusError(w, http.StatusBadRequest, "provider must be claude or azure")
		return
	}

	settings := llm.Settings{
		Endpoint:  r.FormValue("endpoint"),
		Key:       r.FormValue("key"),
		Model:     r.FormValue("model"),
		FastModel: r.FormValue("fast_model"),
	}

	// What the provider said is the person's own account talking back to
	// them ("invalid x-api-key", "deployment not found"), which is what they
	// need to fix the form - not an internal error to hide.
	vision, err := s.LLM.Test(r.Context(), provider, settings)
	if err != nil {
		jsonStatusError(w, http.StatusBadRequest, "Not saved - "+err.Error())
		return
	}
	settings.Vision = vision

	if err := s.LLM.Save(provider, settings); err != nil {
		if errors.Is(err, llm.ErrUnsealed) {
			jsonStatusError(w, http.StatusServiceUnavailable, "Not saved - Decurix could not create the key file it seals "+
				"API keys with ("+llm.KeyFile+" in its data folder), and it does not store a key unencrypted. Make sure "+
				"that folder is writable, then restart Decurix and save again.")
			return
		}
		jsonError(w, err)
		return
	}

	s.llmStatusHandler(w, r)
}

// llmForgetHandler removes a provider's saved settings.
func (s *Server) llmForgetHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodPost) {
		return
	}

	provider := strings.TrimSpace(r.FormValue("provider"))
	if !llm.ValidProvider(provider) {
		http.Error(w, "provider must be claude or azure", http.StatusBadRequest)
		return
	}

	if err := s.LLM.Forget(provider); err != nil {
		jsonError(w, err)
		return
	}

	s.llmStatusHandler(w, r)
}

// llmProxyHandler serves the desktops' agents, over the Docker network, on
// the primary listener only (RegisterLocalRoutes) - nobody holding the gated
// listener's access code gets a free model to spend on:
//
//	/api/llm/chat                           Azure (older desktops' llm_client)
//	/api/llm/<provider>/chat                llm_client
//	/api/llm/<provider>/v1/chat/completions TheBot's OpenAI-compatible provider
//	/api/llm/<provider>/v1/models           TheBot's reachability check
//
// Every provider speaks the OpenAI chat shape here; the key is added (and,
// for Claude, the request translated) by internal/llm.
func (s *Server) llmProxyHandler(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/llm/"), "/")

	provider, route := llm.ProviderAzure, rest
	if first, after, ok := strings.Cut(rest, "/"); ok && llm.ValidProvider(first) {
		provider, route = first, after
	}

	switch route {
	case "chat", "v1/chat/completions":
		s.llmChat(w, r, provider)
	case "v1/models":
		if !s.LLM.Configured(provider) {
			http.Error(w, llm.ErrNotConfigured.Error(), http.StatusServiceUnavailable)
			return
		}
		jsonResponse(w, map[string]any{
			"object": "list",
			"data":   []any{map[string]any{"id": "default", "object": "model", "owned_by": provider}},
		})
	default:
		http.NotFound(w, r)
	}
}

// llmChat forwards one chat request to a provider, adding the key the
// desktop never sees.
func (s *Server) llmChat(w http.ResponseWriter, r *http.Request, provider string) {
	if !allowMethod(w, r, http.MethodPost) {
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

	// A worker at a hard spend limit gets no more hosted calls: the limit is real where the money is spent (C-08).
	worker, fromDesktop := requestDesktop(r)
	if fromDesktop && s.QueueDispatcher != nil {
		why, err := s.QueueDispatcher.OverBudget(worker)
		if err != nil {
			log.Printf("llm %s: reading the spend of %s: %v", provider, worker, err)
			http.Error(w, "the spend limit could not be read, so the call was not made", http.StatusServiceUnavailable)
			return
		}
		if why != "" {
			http.Error(w, "over its spend limit: "+why, http.StatusPaymentRequired)
			return
		}
	}

	status, answer, retry, err := s.LLM.Chat(r.Context(), provider, body)
	if err != nil {
		if errors.Is(err, llm.ErrNotConfigured) {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}

		log.Printf("llm %s: %v", provider, err)
		http.Error(w, "the hosted model could not be reached", http.StatusBadGateway)
		return
	}

	if fromDesktop {
		if err := s.LLM.Meter(worker, time.Now(), llm.ParseUsage(answer)); err != nil {
			log.Printf("llm %s: %v", provider, err)
		}
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

// errBrain is a create request's harness or brain refused, in words for the
// person who chose it.
type errBrain string

func (e errBrain) Error() string { return string(e) }

// desktopBrain checks what a create request asked to reason with. Every new
// desktop runs TheBot ("" or "thebot" for harness; the others are kept only
// for desktops created before); brain is "ollama" (the default), "claude" or
// "azure", and a hosted one must have been set up first - a desktop made
// with a brain nobody configured would fail its first request instead.
func (s *Server) desktopBrain(harness, brain string) (desktop.Brain, error) {
	switch strings.TrimSpace(harness) {
	case "", desktop.HarnessTheBot:
	default:
		return desktop.Brain{}, errBrain("every new desktop runs TheBot: leave harness out and choose its model with brain=ollama, claude, azure or openai")
	}

	switch kind := strings.ToLower(strings.TrimSpace(brain)); kind {
	case "", desktop.BrainOllama:
		return desktop.Brain{Kind: desktop.BrainOllama}, nil
	case desktop.BrainClaude, desktop.BrainAzure, desktop.BrainOpenAI:
		if !s.LLM.Configured(kind) {
			return desktop.Brain{}, errBrain(kind + " is not set up yet: choose it under Model in the New desktop dialog and add its key")
		}
		st := s.LLM.Status()[kind]
		return desktop.Brain{Kind: kind, Model: st.Model, Vision: st.Vision}, nil
	default:
		return desktop.Brain{}, errBrain("brain must be ollama, claude, azure or openai")
	}
}
