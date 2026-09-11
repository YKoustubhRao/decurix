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

func TestOllamaChatParsesRealResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/chat" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "test-model" {
			t.Errorf("request model = %v, want test-model", body["model"])
		}
		opts, _ := body["options"].(map[string]any)
		if opts["num_ctx"] != float64(8192) {
			t.Errorf("num_ctx was not forced to 8192, got %v", opts["num_ctx"])
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message":           map[string]any{"content": "real reply text"},
			"prompt_eval_count": 12,
			"eval_count":        34,
			"total_duration":    5_000_000, // 5ms in ns
		})
	}))
	defer srv.Close()

	provider := NewOllama(srv.URL)
	result, err := provider.Chat(context.Background(), "test-model", []Message{{Role: "user", Content: "hi"}}, Options{})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if result.Text != "real reply text" {
		t.Errorf("Text = %q, want %q", result.Text, "real reply text")
	}
	if result.PromptTokens != 12 || result.ResponseTokens != 34 {
		t.Errorf("token counts = (%d,%d), want (12,34)", result.PromptTokens, result.ResponseTokens)
	}
	if result.DurationMs != 5 {
		t.Errorf("DurationMs = %d, want 5", result.DurationMs)
	}
}

// Temperature must reach Ollama's real options object when the caller sets
// one - the atomizer's planning call relies on this to be deterministic.
func TestOllamaChatForwardsTemperature(t *testing.T) {
	var gotTemp any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		opts, _ := body["options"].(map[string]any)
		gotTemp = opts["temperature"]
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{"content": "ok"}})
	}))
	defer srv.Close()

	provider := NewOllama(srv.URL)
	if _, err := provider.Chat(context.Background(), "test-model", []Message{{Role: "user", Content: "hi"}}, Options{Temperature: 0.15}); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if gotTemp != 0.15 {
		t.Errorf("temperature sent = %v, want 0.15", gotTemp)
	}
}

// A zero Temperature (the field's default) must not be sent at all - that
// leaves Ollama on its own default rather than forcing temperature 0,
// which is a real, different, and more deterministic-than-intended setting.
func TestOllamaChatOmitsZeroTemperature(t *testing.T) {
	var sawKey bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		opts, _ := body["options"].(map[string]any)
		_, sawKey = opts["temperature"]
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{"content": "ok"}})
	}))
	defer srv.Close()

	provider := NewOllama(srv.URL)
	if _, err := provider.Chat(context.Background(), "test-model", []Message{{Role: "user", Content: "hi"}}, Options{}); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if sawKey {
		t.Error("temperature key was sent for an unset (zero) Temperature, want it omitted")
	}
}

func TestOllamaChatSurfacesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"llama-server crashed"}`))
	}))
	defer srv.Close()

	provider := NewOllama(srv.URL)
	_, err := provider.Chat(context.Background(), "test-model", []Message{{Role: "user", Content: "hi"}}, Options{})
	if err == nil {
		t.Fatal("Chat() returned nil error for a real HTTP 500, want an error")
	}
	if !strings.Contains(err.Error(), "llama-server crashed") {
		t.Errorf("error does not surface the real server message: %v", err)
	}
}

func TestOllamaChatSurfacesOllamaLevelError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // Ollama can return 200 with an "error" field inside the body
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "model not found"})
	}))
	defer srv.Close()

	provider := NewOllama(srv.URL)
	_, err := provider.Chat(context.Background(), "test-model", []Message{{Role: "user", Content: "hi"}}, Options{})
	if err == nil || !strings.Contains(err.Error(), "model not found") {
		t.Errorf("expected the real ollama error to surface, got: %v", err)
	}
}

func TestOllamaIsReachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if !NewOllama(srv.URL).IsReachable(context.Background()) {
		t.Error("IsReachable() = false for a real, up server")
	}
	if NewOllama("http://127.0.0.1:1").IsReachable(context.Background()) {
		t.Error("IsReachable() = true for an unreachable address")
	}
}

// TestOllamaChatAgainstRealServer is a real integration test against an
// actual local Ollama - skipped unless one is reachable, so `go test`
// still passes in CI/containers with no GPU/Ollama available, but proves
// genuine end-to-end connectivity whenever this dev environment has one.
func TestOllamaChatAgainstRealServer(t *testing.T) {
	host := os.Getenv("OLLAMA_HOST")
	if host == "" {
		host = "http://127.0.0.1:11434"
	}
	provider := NewOllama(host)
	if !provider.IsReachable(context.Background()) {
		t.Skipf("no real Ollama reachable at %s - skipping live integration test", host)
	}

	model := os.Getenv("TEST_OLLAMA_MODEL")
	if model == "" {
		model = "qwen2.5-coder:14b"
	}
	result, err := provider.Chat(context.Background(), model, []Message{{Role: "user", Content: "Reply with exactly the word: PONG"}}, Options{TimeoutMs: 60000})
	if err != nil {
		t.Fatalf("real Chat() call failed: %v", err)
	}
	if strings.TrimSpace(result.Text) == "" {
		t.Error("real Ollama call returned empty text")
	}
	t.Logf("real Ollama response: %q (prompt=%d response=%d tokens, %dms)", result.Text, result.PromptTokens, result.ResponseTokens, result.DurationMs)
}
