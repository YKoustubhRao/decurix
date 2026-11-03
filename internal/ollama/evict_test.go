package ollama

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAnEmbedThatWouldEvictTheChatModelIsSeen: one model slot and the chat model loaded means an embed now evicts it
// (C-10); with room for two, or the embedder already loaded, it does not.
func TestAnEmbedThatWouldEvictTheChatModelIsSeen(t *testing.T) {
	loaded := `{"models":[{"name":"chat:latest","model":"chat:latest"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(loaded))
	}))
	defer srv.Close()
	t.Setenv("OLLAMA_URL", srv.URL)
	t.Setenv("OLLAMA_MODEL", "chat")
	t.Setenv("OLLAMA_MAX_LOADED_MODELS", "1")
	if !EmbedWouldEvict(context.Background()) {
		t.Fatal("one slot holding the chat model: an embed would evict it")
	}
	t.Setenv("OLLAMA_MAX_LOADED_MODELS", "2")
	if EmbedWouldEvict(context.Background()) {
		t.Fatal("two slots: nothing is evicted")
	}
	t.Setenv("OLLAMA_MAX_LOADED_MODELS", "1")
	// Another chat model loaded (an older desktop's, not this process's pick) is evicted all the same (RW1-E-03).
	loaded = `{"models":[{"name":"other:latest"}]}`
	if !EmbedWouldEvict(context.Background()) {
		t.Fatal("another chat model is loaded: an embed would evict it")
	}
	loaded = `{"models":[{"name":"` + EmbedModel + `"}]}`
	if EmbedWouldEvict(context.Background()) {
		t.Fatal("only the embedder is loaded: nothing is evicted")
	}
	loaded = `{"models":[]}`
	if EmbedWouldEvict(context.Background()) {
		t.Fatal("nothing loaded: nothing is evicted")
	}
}

// TestASeededModelIsTheFirstPicksCurrent: the model existing desktops run is what a fresh pick keeps to (C-11).
func TestASeededModelIsTheFirstPicksCurrent(t *testing.T) {
	auto.mu.Lock()
	old := auto.seed
	auto.seed = ""
	auto.mu.Unlock()
	defer func() { auto.mu.Lock(); auto.seed = old; auto.mu.Unlock() }()
	SeedCurrentModel("qwen3:4b")
	SeedCurrentModel("other")
	auto.mu.Lock()
	got := auto.seed
	auto.mu.Unlock()
	if got != "qwen3:4b" {
		t.Fatalf("seed = %q", got)
	}
}

// TestAFailedShowIsAskedAgain: a non-200 /api/show is no answer, so it is not kept for the process (C-26).
func TestAFailedShowIsAskedAgain(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	installedKVBytes(context.Background(), srv.URL, "c26-model", "")
	installedKVBytes(context.Background(), srv.URL, "c26-model", "")
	if calls != 2 {
		t.Fatalf("asked %d times, want 2: a failure was cached", calls)
	}
}

// TestTheReasonSaysTheShareThatWasUsed: the explanation quotes BudgetFraction, not a fixed 80% (C-21).
func TestTheReasonSaysTheShareThatWasUsed(t *testing.T) {
	t.Setenv("DECURIX_MODEL_BUDGET_FRACTION", "")
	if got := explain(Selection{Fits: true, Model: Candidate{Name: "m"}}, Resources{}); !strings.Contains(got, "50%") {
		t.Fatalf("reason %q", got)
	}
}
