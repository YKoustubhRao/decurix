package rag

import (
	"context"
	"strings"
	"sync"
	"testing"

	"thebot/internal/withheld"
)

// fakeEmbedder keeps every text it is asked to embed.
type fakeEmbedder struct {
	mu    sync.Mutex
	texts []string
}

func (f *fakeEmbedder) Embed(_ context.Context, _ string, text string) ([]float32, error) {
	f.mu.Lock()
	f.texts = append(f.texts, text)
	f.mu.Unlock()
	return []float32{1, 0, 0}, nil
}

// A file the job wrote is indexed with the job's secret values withheld (C14, review M2): neither the index nor the
// embedder sees them.
func TestIngestWithholdsTheJobsSecrets(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "plans.txt", "zip the folder with the password plumtree-cabinet-41, then mail it")
	emb := &fakeEmbedder{}
	r, err := New(root, emb, "unused.py")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := withheld.With(context.Background(), []string{"plumtree-cabinet-41"})
	if _, _, err := r.IngestFile(ctx, "plans.txt"); err != nil {
		t.Fatal(err)
	}
	rows, err := r.db.Query(`SELECT text FROM chunks`)
	if err != nil {
		t.Fatal(err)
	}
	var stored []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		stored = append(stored, s)
	}
	rows.Close()
	all := strings.Join(append(stored, emb.texts...), "\n")
	if len(stored) == 0 || strings.Contains(all, "plumtree") || !strings.Contains(all, "zip the folder") {
		t.Fatalf("indexed %q, embedded %q", stored, emb.texts)
	}
}

// An empty index is queried without the embedder: nothing loads an embedding model for an index with nothing in it
// (resource plan P1 #7); with chunks, the query embeds as before.
func TestAnEmptyIndexIsQueriedWithoutEmbedding(t *testing.T) {
	root := t.TempDir()
	emb := &fakeEmbedder{}
	r, err := New(root, emb, "unused.py")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	results, err := r.Query(context.Background(), "what does the plan say", 3)
	if err != nil || len(results) != 0 || len(emb.texts) != 0 {
		t.Fatalf("results %v, err %v, embedded %q", results, err, emb.texts)
	}
	writeTestFile(t, root, "plan.txt", "the plan says to mail the folder on Monday")
	if _, _, err := r.IngestFile(context.Background(), "plan.txt"); err != nil {
		t.Fatal(err)
	}
	before := len(emb.texts)
	if _, err := r.Query(context.Background(), "what does the plan say", 3); err != nil {
		t.Fatal(err)
	}
	if len(emb.texts) != before+1 {
		t.Fatalf("a query of a filled index did not embed: %d -> %d", before, len(emb.texts))
	}
}
