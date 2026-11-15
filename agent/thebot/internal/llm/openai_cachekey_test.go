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
	learnedRoutes.forget()
	defer learnedRoutes.forget()
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

// A refused prompt_cache_key is a fact of its route (provider, model, endpoint): another model on the same endpoint, or
// the same model at another endpoint, still sends it (routefacts.go).
func TestARefusedCacheKeyIsKeptPerRoute(t *testing.T) {
	learnedRoutes.forget()
	defer learnedRoutes.forget()
	refusing := func(keys *[]string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req map[string]any
			_ = json.NewDecoder(r.Body).Decode(&req)
			key, _ := req["prompt_cache_key"].(string)
			*keys = append(*keys, req["model"].(string)+"="+key)
			if key != "" && req["model"] == "old" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"message":"Unrecognized request argument supplied: prompt_cache_key"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
		}))
	}
	var keysA, keysB []string
	a, b := refusing(&keysA), refusing(&keysB)
	defer a.Close()
	defer b.Close()
	msgs := []Message{{Role: "system", Content: "frame"}, {Role: "user", Content: "go"}}
	pa := &OpenAIProvider{APIKey: "k", Client: a.Client(), BaseURL: a.URL}
	pb := &OpenAIProvider{APIKey: "k", Client: b.Client(), BaseURL: b.URL}
	for _, call := range []struct {
		p     *OpenAIProvider
		model string
	}{{pa, "old"}, {pa, "old"}, {pa, "new"}, {pb, "old"}} {
		if _, err := call.p.Chat(context.Background(), call.model, msgs, Options{}); err != nil {
			t.Fatal(err)
		}
	}
	sentKey := func(entry string) bool { return !strings.HasSuffix(entry, "=") }
	// endpoint A: old (key, refused) -> old without -> old without -> new with a key
	if len(keysA) != 4 || !sentKey(keysA[0]) || sentKey(keysA[1]) || sentKey(keysA[2]) || !sentKey(keysA[3]) {
		t.Errorf("endpoint A sent %q: the refusal must hold for model old there, not for model new", keysA)
	}
	// endpoint B, model old: refused there too, but only learned there after its own refusal
	if len(keysB) < 1 || !sentKey(keysB[0]) {
		t.Errorf("endpoint B sent %q: a refusal at endpoint A must not drop the key at endpoint B", keysB)
	}
}
