package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Found live (2026-10-14): a scanned identity document made Azure refuse the prompt, and the task ended in error.
func TestAContentFilteredRequestIsSentAgainWithoutItsPictures(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if strings.Contains(string(b), "image_url") {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{
				"message": "The response was filtered due to the prompt triggering Azure OpenAI's content management policy."}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": "DONE: read it as text"}}}})
	}))
	defer srv.Close()

	p := &OpenAIProvider{BaseURL: srv.URL, APIKey: "x", Client: srv.Client()}
	msgs := []Message{{Role: "user", Content: "office read id.png", Images: [][]byte{{1, 2, 3}}, ImageLabels: []string{"id.png"}}}
	res, err := p.Chat(context.Background(), "m", msgs, Options{})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if res.Text != "DONE: read it as text" || len(bodies) != 2 {
		t.Fatalf("got %q after %d requests", res.Text, len(bodies))
	}
	if !strings.Contains(bodies[1], "the picture of id.png was left out") {
		t.Errorf("the retry does not say what was left out: %s", bodies[1])
	}
}

func TestOtherRefusalsAreNotRetried(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": "invalid model"}})
	}))
	defer srv.Close()

	p := &OpenAIProvider{BaseURL: srv.URL, APIKey: "x", Client: srv.Client()}
	_, err := p.Chat(context.Background(), "m", []Message{{Role: "user", Content: "x", Images: [][]byte{{1}}}}, Options{})
	if err == nil || calls != 1 {
		t.Fatalf("err=%v calls=%d, want one failed call", err, calls)
	}
}
