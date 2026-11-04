package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"thebot/internal/llm"
)

// /api/health says who embeds for this run, so the agent warms an embedding model only when one is used (resource
// plan P1 #7): the local Ollama, the hosted provider itself, or none.
func TestHealthSaysWhoEmbeds(t *testing.T) {
	var none llm.Embedder
	for e, want := range map[llm.Embedder]string{none: "off", llm.NewOllama("http://x"): "ollama", llm.NewOpenAI("k", ""): "openai"} {
		if got := embeddingsOf(e); got != want {
			t.Errorf("embeddingsOf(%T) = %q, want %q", e, got, want)
		}
	}
	s := newTestServer(t)
	s.embeddings = "off"
	rec := serve(s, http.MethodGet, "/api/health", "")
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["embeddings"] != "off" {
		t.Fatalf("health = %s (%v)", rec.Body.String(), err)
	}
}
