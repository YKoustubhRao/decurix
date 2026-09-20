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
		if body["stream"] != true {
			t.Errorf("stream = %v, want true - a non-streaming call sends zero bytes until the whole answer is ready, and any proxy in between reads that silence as a dead origin (found live: a Cloudflare tunnel killed every real code-generation call at 100s with HTTP 524)", body["stream"])
		}
		// Ollama's real streaming shape: newline-delimited JSON, one chunk
		// per fragment, counters only on the final done chunk.
		w.Header().Set("Content-Type", "application/x-ndjson")
		enc := json.NewEncoder(w)
		_ = enc.Encode(map[string]any{"message": map[string]any{"content": "real reply "}, "done": false})
		_ = enc.Encode(map[string]any{"message": map[string]any{"content": "text"}, "done": false})
		_ = enc.Encode(map[string]any{
			"message":           map[string]any{"content": ""},
			"done":              true,
			"done_reason":       "stop",
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
		t.Errorf("Text = %q, want %q - every streamed fragment must be accumulated", result.Text, "real reply text")
	}
	if result.Truncated {
		t.Error("Truncated = true for a normal stop, want false")
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
// actual local Ollama - opt-in: set TEST_OLLAMA_MODEL to a model that server
// has. It used to run whenever an Ollama answered, with a default model that
// is usually not installed, and worse: a chat with different options makes
// Ollama evict the model every desktop is using and load another runner -
// minutes on a CPU host - so a plain `go test` could freeze the agents.
func TestOllamaChatAgainstRealServer(t *testing.T) {
	model := os.Getenv("TEST_OLLAMA_MODEL")
	if model == "" {
		t.Skip("set TEST_OLLAMA_MODEL to run the live Ollama integration test")
	}

	host := os.Getenv("OLLAMA_HOST")
	if host == "" {
		host = "http://127.0.0.1:11434"
	}
	provider := NewOllama(host)
	if !provider.IsReachable(context.Background()) {
		t.Skipf("no real Ollama reachable at %s - skipping live integration test", host)
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

// A response cut off by the token ceiling arrives as a clean HTTP 200 with
// a body that looks like any other answer - the ONLY signal is
// done_reason. Without surfacing it, a truncated source file is
// indistinguishable from badly written code, and the coding harness spends
// its whole budget "fixing" a file that was never finished.
func TestOllamaChatReportsTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enc := json.NewEncoder(w)
		_ = enc.Encode(map[string]any{"message": map[string]any{"content": "def add(a, b):\n    result = a +"}, "done": false})
		_ = enc.Encode(map[string]any{
			"message": map[string]any{"content": ""}, "done": true, "done_reason": "length",
		})
	}))
	defer srv.Close()

	result, err := NewOllama(srv.URL).Chat(context.Background(), "m", []Message{{Role: "user", Content: "hi"}}, Options{})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if !result.Truncated {
		t.Error("Truncated = false for done_reason=length, want true - this is the only way a cut-off file can be told apart from a wrong one")
	}
	if result.Text == "" {
		t.Error("Text is empty, want the real partial content kept (it is still what the model produced)")
	}
}

// Code generation needs a much bigger window than a chat turn, and a
// caller asking for one must actually get it - the default 8192 shared
// between a large prompt and a whole source file is what silently
// truncated real files.
func TestOllamaChatForwardsNumCtxAndMaxTokens(t *testing.T) {
	var gotCtx, gotPredict any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		opts, _ := body["options"].(map[string]any)
		gotCtx, gotPredict = opts["num_ctx"], opts["num_predict"]
		_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{"content": "ok"}, "done": true})
	}))
	defer srv.Close()

	if _, err := NewOllama(srv.URL).Chat(context.Background(), "m",
		[]Message{{Role: "user", Content: "hi"}}, Options{NumCtx: 32768, MaxTokens: 8192}); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if gotCtx != float64(32768) {
		t.Errorf("num_ctx sent = %v, want 32768", gotCtx)
	}
	if gotPredict != float64(8192) {
		t.Errorf("num_predict sent = %v, want 8192", gotPredict)
	}
}

func TestOllamaChatForwardsStopAndThreadOptions(t *testing.T) {
	var gotStop any
	var gotThread, gotBatch any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		opts, _ := body["options"].(map[string]any)
		gotStop = opts["stop"]
		gotThread = opts["num_thread"]
		gotBatch = opts["num_batch"]
		_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{"content": "ok"}, "done": true})
	}))
	defer srv.Close()

	stopSeq := []string{"\nCommand execution result:"}
	if _, err := NewOllama(srv.URL).Chat(context.Background(), "m",
		[]Message{{Role: "user", Content: "hi"}}, Options{Stop: stopSeq}); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}

	stopList, ok := gotStop.([]any)
	if !ok || len(stopList) != 1 || stopList[0] != stopSeq[0] {
		t.Errorf("stop sequence sent = %v, want %v", gotStop, stopSeq)
	}
	if gotThread == nil || gotThread.(float64) <= 0 {
		t.Errorf("num_thread sent = %v, want > 0", gotThread)
	}
	if gotBatch != float64(512) {
		t.Errorf("num_batch sent = %v, want 512", gotBatch)
	}
}
