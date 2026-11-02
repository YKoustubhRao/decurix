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

func TestAnthropicChatParsesRealResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "test-key" {
			t.Errorf("x-api-key = %q, want test-key", r.Header.Get("x-api-key"))
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Error("anthropic-version header not set")
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["system"] != "you are helpful" {
			t.Errorf("system = %v, want the leading system message pulled out of messages[]", body["system"])
		}
		msgs, _ := body["messages"].([]any)
		if len(msgs) != 1 {
			t.Errorf("messages length = %d, want 1 (system message should have been removed from the array)", len(msgs))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{{"type": "text", "text": "real reply"}},
			"usage":   map[string]any{"input_tokens": 10, "output_tokens": 5},
		})
	}))
	defer srv.Close()

	p := NewAnthropic("test-key", "claude-test")
	p.BaseURL = srv.URL
	result, err := p.Chat(context.Background(), "claude-test", []Message{
		{Role: "system", Content: "you are helpful"},
		{Role: "user", Content: "hi"},
	}, Options{})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if result.Text != "real reply" {
		t.Errorf("Text = %q, want %q", result.Text, "real reply")
	}
	if result.PromptTokens != 10 || result.ResponseTokens != 5 {
		t.Errorf("token counts = (%d,%d), want (10,5)", result.PromptTokens, result.ResponseTokens)
	}
}

func TestAnthropicChatSurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "invalid x-api-key"}})
	}))
	defer srv.Close()

	p := NewAnthropic("bad-key", "claude-test")
	p.BaseURL = srv.URL
	_, err := p.Chat(context.Background(), "claude-test", []Message{{Role: "user", Content: "hi"}}, Options{})
	if err == nil || !strings.Contains(err.Error(), "invalid x-api-key") {
		t.Errorf("expected the real Anthropic error to surface, got: %v", err)
	}
}

func TestAnthropicIsReachableFalseWithoutKey(t *testing.T) {
	p := NewAnthropic("", "claude-test")
	if p.IsReachable(context.Background()) {
		t.Error("IsReachable() = true with an empty API key, want false")
	}
}

func TestAnthropicHasNoEmbedder(t *testing.T) {
	p := NewAnthropic("test-key", "claude-test")
	if _, ok := any(p).(Embedder); ok {
		t.Error("AnthropicProvider implements Embedder, but Anthropic has no real embeddings API - this would silently break the Data RAG")
	}
}

// TestAnthropicChatAgainstRealServer is a real integration test - skipped
// unless ANTHROPIC_API_KEY is actually set, so `go test` still passes
// without one.
func TestAnthropicChatAgainstRealServer(t *testing.T) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY not set - skipping live integration test")
	}
	p := NewAnthropic(key, "claude-3-5-haiku-latest")
	result, err := p.Chat(context.Background(), "claude-3-5-haiku-latest", []Message{{Role: "user", Content: "Reply with exactly the word: PONG"}}, Options{TimeoutMs: 30000})
	if err != nil {
		t.Fatalf("real Chat() call failed: %v", err)
	}
	if strings.TrimSpace(result.Text) == "" {
		t.Error("real Anthropic call returned empty text")
	}
	t.Logf("real Anthropic response: %q", result.Text)
}

// stop_reason max_tokens is an answer cut off for want of room.
func TestAnthropicChatMarksACutOffAnswer(t *testing.T) {
	for reason, want := range map[string]bool{"max_tokens": true, "end_turn": false} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"content": []map[string]any{{"type": "text", "text": "half a rep"}}, "stop_reason": reason,
			})
		}))
		p := NewAnthropic("test-key", "claude-test")
		p.BaseURL = srv.URL
		result, err := p.Chat(context.Background(), "claude-test", []Message{{Role: "user", Content: "hi"}}, Options{})
		srv.Close()
		if err != nil || result.Truncated != want {
			t.Errorf("stop_reason %q: Truncated = %v (%v), want %v", reason, result.Truncated, err, want)
		}
	}
}
