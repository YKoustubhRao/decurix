package suggest

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/memory"
	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
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

// TestCacheMissingVectorsBoundsBackfillPerCall covers a cold cache: a large
// batch of memory entries with no cached embedding yet must not turn one
// Suggest call into dozens of sequential Ollama round trips inside the
// caller's ctx budget (internal/httpapi's suggestTimeout) - the cache still
// fills in, just across more than one call.
func TestCacheMissingVectorsBoundsBackfillPerCall(t *testing.T) {
	desktops := &fakeDesktops{list: []desktop.Desktop{{ID: "d1", Name: "d1"}}}

	entries := make([]memory.WorkerMemoryEntry, 0, maxBackfillPerCall+5)
	for i := 0; i < maxBackfillPerCall+5; i++ {
		entries = append(entries, memory.WorkerMemoryEntry{
			ID:        fmt.Sprintf("m%d", i),
			DesktopID: "d1",
			Title:     fmt.Sprintf("title-%d", i),
			Summary:   "summary",
		})
	}

	var embedCalls int
	embed := func(_ context.Context, _ string) ([]float32, error) {
		embedCalls++
		return []float32{1, 0}, nil
	}

	s := newTestStore(t, desktops, &fakeActivity{}, &fakeMemory{entries: entries}, embed)

	s.Suggest(context.Background(), "deploy the app to staging")

	// 1 call for the request text itself, plus at most maxBackfillPerCall
	// for the memory entries.
	if want := maxBackfillPerCall + 1; embedCalls > want {
		t.Fatalf("embed called %d times in one Suggest call, want at most %d", embedCalls, want)
	}

	embedCalls = 0
	s.Suggest(context.Background(), "deploy the app to staging")

	if embedCalls == 0 {
		t.Fatal("expected a second call to keep backfilling the entries the first call left uncached")
	}
}

func TestNormalizeVectorAndFastDotProduct(t *testing.T) {
	v1 := []float32{3, 4}
	norm1 := normalizeVector(v1)

	// Length should be 1.0 (3/5 = 0.6, 4/5 = 0.8; 0.6^2 + 0.8^2 = 1.0)
	sumSq := float64(norm1[0]*norm1[0] + norm1[1]*norm1[1])
	if math.Abs(sumSq-1.0) > 1e-5 {
		t.Fatalf("expected norm 1.0, got %f", sumSq)
	}

	v2 := []float32{0, 5}
	norm2 := normalizeVector(v2)

	// Dot product: 0.6*0 + 0.8*1 = 0.8
	sim := cosineSimilarity(norm1, norm2)
	if math.Abs(sim-0.8) > 1e-4 {
		t.Fatalf("expected cosine similarity ~0.8, got %f", sim)
	}

	// Test zero vector handling
	zero := []float32{0, 0}
	if normZero := normalizeVector(zero); len(normZero) != 2 || normZero[0] != 0 {
		t.Fatalf("expected zero vector unchanged, got %v", normZero)
	}
	if simZero := cosineSimilarity(norm1, zero); simZero != 0 {
		t.Fatalf("expected 0 for zero vector similarity, got %f", simZero)
	}
}

// A deleted desktop's memory entries are gone; their cached embeddings go too.
func TestForgetDropsOnlyTheNamedEmbeddings(t *testing.T) {
	s := newTestStore(t, &fakeDesktops{}, &fakeActivity{}, &fakeMemory{}, (&stubEmbedder{}).embed)

	for _, id := range []string{"m1", "m2", "m3"} {
		if _, err := s.db.Exec(`INSERT INTO suggestion_embeddings (memory_id, vector, dims, created_ns) VALUES (?, ?, 1, 1)`, id, []byte{0, 0, 0, 0}); err != nil {
			t.Fatal(err)
		}
	}

	s.Forget(nil)
	s.Forget([]string{"m1", "m3", "never-cached"})

	rows, err := s.db.Query(`SELECT memory_id FROM suggestion_embeddings`)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		left = append(left, id)
	}
	rows.Close()

	if len(left) != 1 || left[0] != "m2" {
		t.Fatalf("expected only m2's embedding left, got %v", left)
	}
}
