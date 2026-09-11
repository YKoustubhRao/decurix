package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestOpenAIChatParsesRealResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("Authorization = %q, want Bearer test-key", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "real reply"}}},
			"usage":   map[string]any{"prompt_tokens": 8, "completion_tokens": 4},
		})
	}))
	defer srv.Close()

	p := NewOpenAI("test-key", "")
	p.BaseURL = srv.URL
	result, err := p.Chat(context.Background(), "gpt-4o", []Message{{Role: "user", Content: "hi"}}, Options{})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if result.Text != "real reply" {
		t.Errorf("Text = %q, want %q", result.Text, "real reply")
	}
	if result.PromptTokens != 8 || result.ResponseTokens != 4 {
		t.Errorf("token counts = (%d,%d), want (8,4)", result.PromptTokens, result.ResponseTokens)
	}
}

func TestOpenAIChatSurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "invalid api key"}})
	}))
	defer srv.Close()

	p := NewOpenAI("bad-key", "")
	p.BaseURL = srv.URL
	_, err := p.Chat(context.Background(), "gpt-4o", []Message{{Role: "user", Content: "hi"}}, Options{})
	if err == nil || !strings.Contains(err.Error(), "invalid api key") {
		t.Errorf("expected the real OpenAI error to surface, got: %v", err)
	}
}

func TestOpenAIEmbedParsesRealResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "text-embedding-3-small" {
			t.Errorf("model = %v, want the default embed model when an empty model is passed", body["model"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"embedding": []float64{0.1, 0.2, 0.3}}},
		})
	}))
	defer srv.Close()

	p := NewOpenAI("test-key", "")
	p.BaseURL = srv.URL
	vec, err := p.Embed(context.Background(), "", "some text")
	if err != nil {
		t.Fatalf("Embed() error = %v", err)
	}
	if len(vec) != 3 {
		t.Errorf("vector length = %d, want 3", len(vec))
	}
}

func TestOpenAIIsReachableFalseWithoutKey(t *testing.T) {
	p := NewOpenAI("", "")
	if p.IsReachable(context.Background()) {
		t.Error("IsReachable() = true with an empty API key, want false")
	}
}

// TestOpenAIChatAgainstRealServer is a real integration test - skipped
// unless OPENAI_API_KEY is actually set.
func TestOpenAIChatAgainstRealServer(t *testing.T) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Skip("OPENAI_API_KEY not set - skipping live integration test")
	}
	p := NewOpenAI(key, "")
	result, err := p.Chat(context.Background(), "gpt-4o-mini", []Message{{Role: "user", Content: "Reply with exactly the word: PONG"}}, Options{TimeoutMs: 30000})
	if err != nil {
		t.Fatalf("real Chat() call failed: %v", err)
	}
	if strings.TrimSpace(result.Text) == "" {
		t.Error("real OpenAI call returned empty text")
	}
	t.Logf("real OpenAI response: %q", result.Text)
}
