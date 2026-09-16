package suggest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/YKoustubhRao/flowera/internal/desktop"
	"github.com/YKoustubhRao/flowera/internal/memory"
	"github.com/YKoustubhRao/flowera/internal/sqlitedb"
)

type fakeDesktops struct {
	list   []desktop.Desktop
	viewed map[string]time.Time
}

func (f *fakeDesktops) List(context.Context) ([]desktop.Desktop, error) { return f.list, nil }

func (f *fakeDesktops) LastViewed(id string) (time.Time, bool) {
	t, ok := f.viewed[id]
	return t, ok
}

type fakeActivity struct {
	activity map[string]time.Time
}

func (f *fakeActivity) LastActivity(workerID string) (time.Time, bool, error) {
	t, ok := f.activity[workerID]
	return t, ok, nil
}

type fakeMemory struct {
	entries []memory.WorkerMemoryEntry
}

func (f *fakeMemory) List(desktopID string) []memory.WorkerMemoryEntry {
	if desktopID == "" {
		return f.entries
	}

	var out []memory.WorkerMemoryEntry
	for _, e := range f.entries {
		if e.DesktopID == desktopID {
			out = append(out, e)
		}
	}
	return out
}

// stubEmbedder maps known text to a fixed vector, so tests can assert exact
// similarity without a real Ollama. Unknown text (a cache miss on an entry
// this test never registered) returns an error, matching a real embed
// failure.
type stubEmbedder struct {
	vectors map[string][]float32
	err     error
}

func (s *stubEmbedder) embed(_ context.Context, text string) ([]float32, error) {
	if s.err != nil {
		return nil, s.err
	}

	v, ok := s.vectors[text]
	if !ok {
		return nil, errors.New("stub: no vector registered for text")
	}

	return v, nil
}

func newTestStore(t *testing.T, desktops DesktopLister, tasksStore ActivityStore, memoryStore MemoryLister, embed Embedder) *Store {
	t.Helper()

	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	s, err := NewStore(db, desktops, tasksStore, memoryStore, embed)
	if err != nil {
		t.Fatal(err)
	}

	return s
}

func TestSuggestNoDesktops(t *testing.T) {
	s := newTestStore(t, &fakeDesktops{}, &fakeActivity{}, &fakeMemory{}, nil)

	if _, ok := s.Suggest(context.Background(), "deploy the app to staging"); ok {
		t.Fatal("expected no suggestion with no desktops")
	}
}

func TestSuggestFallsBackToRecencyWithNoMemory(t *testing.T) {
	now := time.Now()

	desktops := &fakeDesktops{
		list: []desktop.Desktop{
			{ID: "a", Name: "alpha", StartedAt: now.Add(-time.Hour).Format(time.RFC3339)},
			{ID: "b", Name: "bravo", StartedAt: now.Format(time.RFC3339)},
		},
	}

	s := newTestStore(t, desktops, &fakeActivity{}, &fakeMemory{}, nil)

	got, ok := s.Suggest(context.Background(), "deploy the app to staging please")
	if !ok {
		t.Fatal("expected a recency suggestion")
	}

	if got.DesktopID != "b" || got.Reason != "recent" {
		t.Fatalf("expected bravo via recency, got %+v", got)
	}
}

func TestSuggestFallsBackToRecencyWhenContentTooShort(t *testing.T) {
	now := time.Now()

	desktops := &fakeDesktops{
		list: []desktop.Desktop{
			{ID: "a", Name: "alpha", StartedAt: now.Format(time.RFC3339)},
		},
	}

	embed := &stubEmbedder{vectors: map[string][]float32{}}

	s := newTestStore(t, desktops, &fakeActivity{}, &fakeMemory{}, embed.embed)

	got, ok := s.Suggest(context.Background(), "hi")
	if !ok || got.Reason != "recent" {
		t.Fatalf("expected recency fallback for short content, got %+v ok=%v", got, ok)
	}
}

func TestSuggestPrefersConfidentMemoryMatch(t *testing.T) {
	now := time.Now()

	desktops := &fakeDesktops{
		list: []desktop.Desktop{
			{ID: "a", Name: "alpha", StartedAt: now.Format(time.RFC3339)},
			{ID: "b", Name: "bravo", StartedAt: now.Add(-time.Hour).Format(time.RFC3339)},
		},
	}

	entries := &fakeMemory{entries: []memory.WorkerMemoryEntry{
		{ID: "m1", DesktopID: "b", Title: "Deploy to staging", Summary: "kubectl apply against the staging cluster"},
	}}

	entryText := entries.entries[0].Title + "\n" + entries.entries[0].Summary
	requestText := "please deploy the latest build to staging"

	embed := &stubEmbedder{vectors: map[string][]float32{
		entryText:   {1, 0, 0},
		requestText: {0.9, 0.1, 0},
	}}

	s := newTestStore(t, desktops, &fakeActivity{}, entries, embed.embed)

	got, ok := s.Suggest(context.Background(), requestText)
	if !ok {
		t.Fatal("expected a suggestion")
	}

	if got.DesktopID != "b" || got.Reason != "memory" {
		t.Fatalf("expected bravo via memory match despite alpha being more recent, got %+v", got)
	}

	// The embedding is now cached, so a second call must not need the entry's
	// text registered in the embedder again - only the (unchanged) request
	// text's vector is looked up.
	embed.vectors = map[string][]float32{requestText: {0.9, 0.1, 0}}

	got2, ok := s.Suggest(context.Background(), requestText)
	if !ok || got2.DesktopID != "b" {
		t.Fatalf("expected cached memory match on second call, got %+v ok=%v", got2, ok)
	}
}

func TestSuggestFallsBackWhenMemoryScoreTooLow(t *testing.T) {
	now := time.Now()

	desktops := &fakeDesktops{
		list: []desktop.Desktop{
			{ID: "a", Name: "alpha", StartedAt: now.Format(time.RFC3339)},
			{ID: "b", Name: "bravo", StartedAt: now.Add(-time.Hour).Format(time.RFC3339)},
		},
	}

	entries := &fakeMemory{entries: []memory.WorkerMemoryEntry{
		{ID: "m1", DesktopID: "b", Title: "Unrelated workflow", Summary: "formats spreadsheets"},
	}}

	entryText := entries.entries[0].Title + "\n" + entries.entries[0].Summary
	requestText := "please deploy the latest build to staging"

	// Orthogonal vectors: cosine similarity 0, well under minConfidence.
	embed := &stubEmbedder{vectors: map[string][]float32{
		entryText:   {1, 0, 0},
		requestText: {0, 1, 0},
	}}

	s := newTestStore(t, desktops, &fakeActivity{}, entries, embed.embed)

	got, ok := s.Suggest(context.Background(), requestText)
	if !ok || got.Reason != "recent" || got.DesktopID != "a" {
		t.Fatalf("expected recency fallback (alpha) on a weak match, got %+v ok=%v", got, ok)
	}
}

func TestSuggestDegradesGracefullyWhenEmbeddingFails(t *testing.T) {
	now := time.Now()

	desktops := &fakeDesktops{
		list: []desktop.Desktop{
			{ID: "a", Name: "alpha", StartedAt: now.Format(time.RFC3339)},
		},
	}

	entries := &fakeMemory{entries: []memory.WorkerMemoryEntry{
		{ID: "m1", DesktopID: "a", Title: "Deploy to staging", Summary: "kubectl apply"},
	}}

	embed := &stubEmbedder{err: errors.New("ollama unreachable")}

	s := newTestStore(t, desktops, &fakeActivity{}, entries, embed.embed)

	got, ok := s.Suggest(context.Background(), "please deploy the latest build to staging")
	if !ok || got.Reason != "recent" {
		t.Fatalf("expected a graceful recency fallback when embedding fails, got %+v ok=%v", got, ok)
	}
}

func TestRecencyPickUsesTaskAndViewSignals(t *testing.T) {
	now := time.Now()

	desktops := &fakeDesktops{
		list: []desktop.Desktop{
			// alpha has no StartedAt signal at all, but was viewed most recently.
			{ID: "a", Name: "alpha", CreatedAt: now.Add(-48 * time.Hour).Format(time.RFC3339)},
			{ID: "b", Name: "bravo", StartedAt: now.Add(-2 * time.Hour).Format(time.RFC3339)},
		},
		viewed: map[string]time.Time{"a": now},
	}

	tasksStore := &fakeActivity{activity: map[string]time.Time{"b": now.Add(-90 * time.Minute)}}

	s := newTestStore(t, desktops, tasksStore, &fakeMemory{}, nil)

	got, ok := s.Suggest(context.Background(), "short")
	if !ok || got.DesktopID != "a" {
		t.Fatalf("expected alpha via LastViewed beating bravo's task activity, got %+v ok=%v", got, ok)
	}
}
