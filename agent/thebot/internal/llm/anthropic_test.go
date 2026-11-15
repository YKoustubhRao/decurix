package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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

// Anthropic has no "system" role inside messages[]: several system-role messages are joined into the top-level
// `system` field, never sent as a messages[] entry (which the API rejects).
func TestAnthropicMergesEverySystemMessage(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		json.NewEncoder(w).Encode(map[string]any{"content": []map[string]any{{"type": "text", "text": "ok"}}})
	}))
	defer srv.Close()
	p := &AnthropicProvider{APIKey: "k", Client: srv.Client(), BaseURL: srv.URL}
	_, err := p.Chat(context.Background(), "claude-test", []Message{
		{Role: "system", Content: "frame one"},
		{Role: "user", Content: "hi"},
		{Role: "system", Content: "frame two"},
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if body["system"] != "frame one\n\nframe two" {
		t.Errorf("system = %v, want both system messages joined", body["system"])
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages length = %d, want 1 (no system-role message left in the array)", len(msgs))
	}
	if m, _ := msgs[0].(map[string]any); m["role"] != "user" {
		t.Errorf("remaining message role = %v, want user", m["role"])
	}
}

// IsReachable never makes a chat call - /api/health polls it (review T-25): it asks the Models API about the model.
func TestAnthropicIsReachableMakesNoChatCall(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		if r.Header.Get("x-api-key") != "k" || r.URL.Path != "/v1/models/claude-test" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"id": "claude-test"}`))
	}))
	defer srv.Close()
	p := NewAnthropic("k", "claude-test")
	p.BaseURL = srv.URL
	if !p.IsReachable(context.Background()) {
		t.Fatalf("not reachable: %v", paths)
	}
	for _, path := range paths {
		if strings.Contains(path, "/v1/messages") || !strings.HasPrefix(path, "GET ") {
			t.Fatalf("a health check made a chat call: %v", paths)
		}
	}
	unknown := NewAnthropic("k", "no-such-model")
	unknown.BaseURL = srv.URL
	if unknown.IsReachable(context.Background()) {
		t.Error("an unknown model reads as reachable")
	}
}

// anthropicCapture answers every call and keeps each raw request body.
func anthropicCapture(t *testing.T, usage map[string]any) (*AnthropicProvider, *[][]byte) {
	t.Helper()
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, b)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{{"type": "text", "text": "ok"}}, "stop_reason": "end_turn", "usage": usage,
		})
	}))
	t.Cleanup(srv.Close)
	p := NewAnthropic("k", "claude-test")
	p.BaseURL = srv.URL
	return p, &bodies
}

// A conversation is marked for Claude's cache at the system prompt and its three newest messages (four breakpoints,
// the most the API takes); a one-shot call is not marked at all, and its body is what it was before the marks.
func TestClaudeCacheBreakpointsAreTheSystemAndARollingThree(t *testing.T) {
	p, bodies := anthropicCapture(t, map[string]any{"input_tokens": 50, "output_tokens": 3,
		"cache_read_input_tokens": 9000, "cache_creation_input_tokens": 400})
	oneShot := []Message{{Role: "system", Content: "frame"}, {Role: "user", Content: "classify this"}}
	turns := []Message{{Role: "system", Content: "frame"}, {Role: "user", Content: "the request"},
		{Role: "assistant", Content: "COMMAND: ls"}, {Role: "user", Content: "a.txt"},
		{Role: "assistant", Content: "COMMAND: gui observe"},
		{Role: "user", Content: "the screen", Images: [][]byte{[]byte("\x89PNG\r\n\x1a\nxx")}}}
	for _, msgs := range [][]Message{oneShot, turns, turns} {
		if _, err := p.Chat(context.Background(), "claude-test", msgs, Options{}); err != nil {
			t.Fatal(err)
		}
	}
	one := string((*bodies)[0])
	if strings.Contains(one, "cache_control") || !strings.Contains(one, `"system":"frame"`) {
		t.Errorf("a one-shot call was marked, or its body changed: %s", one)
	}
	var sent struct {
		System   []map[string]any `json:"system"`
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal((*bodies)[1], &sent); err != nil {
		t.Fatal(err)
	}
	if len(sent.System) != 1 || sent.System[0]["cache_control"] == nil || sent.System[0]["text"] != "frame" {
		t.Errorf("system not marked: %+v", sent.System)
	}
	for i, m := range sent.Messages {
		marked := strings.Count(string(m.Content), "cache_control")
		want := 0
		if i >= len(sent.Messages)-cacheBreakpointMessages {
			want = 1
		}
		if marked != want {
			t.Errorf("message %d has %d marks, want %d: %s", i, marked, want, m.Content)
		}
	}
	last := string(sent.Messages[len(sent.Messages)-1].Content)
	if !strings.HasSuffix(strings.TrimSpace(last), `"cache_control":{"type":"ephemeral"},"text":"the screen","type":"text"}]`) {
		t.Errorf("the picture message is not marked at its closing text: %s", last)
	}
	if strings.Count(string((*bodies)[1]), "cache_control") != 4 {
		t.Errorf("want four breakpoints in all: %s", (*bodies)[1])
	}
	if !bytes.Equal((*bodies)[1], (*bodies)[2]) {
		t.Error("the same conversation gave two different request bodies")
	}
}

// Claude's usage counts cached and cache-written prompt beside input_tokens: all of it is prompt, the read part cached.
func TestClaudeCountsTheWholePromptAndSaysWhatWasCached(t *testing.T) {
	p, _ := anthropicCapture(t, map[string]any{"input_tokens": 50, "output_tokens": 3,
		"cache_read_input_tokens": 9000, "cache_creation_input_tokens": 400})
	res, err := p.Chat(context.Background(), "claude-test", []Message{{Role: "user", Content: "hi"}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.PromptTokens != 9450 || res.CachedTokens != 9000 || res.FinishReason != "end_turn" {
		t.Fatalf("result %+v", res)
	}
}
