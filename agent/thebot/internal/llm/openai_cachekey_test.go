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

func TestOneGoalsCallsShareACacheKeyAndARefusedKeyIsDropped(t *testing.T) {
	a := cacheKey([]openaiMessage{{Role: "system", Content: "frame"}, {Role: "user", Content: "file September"}, {Role: "user", Content: "step 1"}})
	b := cacheKey([]openaiMessage{{Role: "system", Content: "frame"}, {Role: "user", Content: "file September"}, {Role: "user", Content: "step 2"}})
	c := cacheKey([]openaiMessage{{Role: "system", Content: "frame"}, {Role: "user", Content: "file October"}})
	if a == "" || a != b || a == c {
		t.Fatalf("keys %q %q %q: one goal must share one key, another goal its own", a, b, c)
	}

	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		key, _ := req["prompt_cache_key"].(string)
		keys = append(keys, key)
		if key != "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Unrecognized request argument supplied: prompt_cache_key"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
	}))
	defer srv.Close()
	defer noCacheKey.Store(false)
	o := &OpenAIProvider{APIKey: "k", Client: srv.Client(), BaseURL: srv.URL}
	for i := 0; i < 2; i++ {
		res, err := o.Chat(context.Background(), "m", []Message{{Role: "system", Content: "frame"}, {Role: "user", Content: "go"}}, Options{})
		if err != nil || res.Text != "OK" {
			t.Fatalf("call %d: %v %q", i, err, res.Text)
		}
	}
	if len(keys) != 3 || keys[0] == "" || keys[1] != "" || keys[2] != "" || !strings.Contains(strings.Join(keys, ","), keys[0]) {
		t.Errorf("keys sent %q: the refused key must be dropped for the retry and after", keys)
	}
}
