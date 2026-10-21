package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The provider's cached share of a prompt is read, and summed per job: what makes a repeated prefix cheap is measured.
func TestOpenAIChatReadsTheCachedShareAndTheMeterSumsIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "ok"}}},
			"usage": map[string]any{"prompt_tokens": 9000, "completion_tokens": 40,
				"prompt_tokens_details": map[string]any{"cached_tokens": 8704}},
		})
	}))
	defer srv.Close()
	p := NewOpenAI("k", "")
	p.BaseURL = srv.URL
	ctx, usage := WithUsage(context.Background())
	m := Metered(p)
	for i := 0; i < 2; i++ {
		res, err := m.Chat(ctx, "gpt", []Message{{Role: "user", Content: "hi"}}, Options{})
		if err != nil || res.CachedTokens != 8704 || res.PromptTokens != 9000 {
			t.Fatalf("result = %+v, %v", res, err)
		}
	}
	if prompt, _, calls := usage.Snapshot(); prompt != 18000 || calls != 2 || usage.Cached() != 17408 {
		t.Errorf("usage = %d prompt, %d calls, %d cached", prompt, calls, usage.Cached())
	}
}
