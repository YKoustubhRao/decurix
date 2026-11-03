package suggest

import (
	"context"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/memory"
)

// When embedding would push the chat model out of a one-model Ollama, a suggestion embeds nothing and falls back to
// recency (C-10): a minutes-long reload on the next agent turn costs more than a better guess saves.
func TestNoEmbeddingWhenItWouldEvictTheChatModel(t *testing.T) {
	now := time.Now()
	desktops := &fakeDesktops{list: []desktop.Desktop{
		{ID: "a", Name: "alpha", StartedAt: now.Format(time.RFC3339)},
		{ID: "b", Name: "bravo", StartedAt: now.Add(-time.Hour).Format(time.RFC3339)},
	}}
	entries := &fakeMemory{entries: []memory.WorkerMemoryEntry{{ID: "m1", DesktopID: "b", Title: "Deploy", Summary: "staging"}}}

	calls := 0
	embed := func(context.Context, string) ([]float32, error) { calls++; return []float32{1, 0}, nil }
	s := newTestStore(t, desktops, &fakeActivity{}, entries, embed)
	s.WouldEvict = func(context.Context) bool { return true }

	got, ok := s.Suggest(context.Background(), "please deploy the latest build to staging")
	if !ok || got.Reason != "recent" || calls != 0 {
		t.Fatalf("got %+v ok=%v with %d embeddings, want recency and none", got, ok, calls)
	}
}
