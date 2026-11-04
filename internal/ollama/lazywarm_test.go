package ollama

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// Readying the Ollama the desktops use loads no model (resource plan P1 #7): the chat model's weights are loaded the
// first time something is about to need them (WarmAsync), never held from boot.
func TestReadyingOllamaLoadsNoModel(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer server.Close()
	t.Setenv("OLLAMA_URL", server.URL)
	t.Setenv("OLLAMA_MODEL", "llama3.2:1b")
	t.Setenv("DECURIX_MODEL_AUTOPULL", "")
	if err := NewManager(nil, "unused").EnsureReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, p := range paths {
		if p == "POST /api/generate" || p == "POST /api/chat" {
			t.Fatalf("readying Ollama loaded a model: %v", paths)
		}
	}
}
