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
