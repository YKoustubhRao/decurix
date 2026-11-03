package rag

import (
	"context"
	"strings"
	"testing"
	"time"
)

// slowEmbedder takes its time on every chunk and says when it has started.
type slowEmbedder struct {
	started chan struct{}
	delay   time.Duration
}

func (e *slowEmbedder) Embed(ctx context.Context, model, text string) ([]float32, error) {
	select {
	case e.started <- struct{}{}:
	default:
	}
	time.Sleep(e.delay)
	return []float32{1, 0}, nil
}

// An ingest never holds the index while it embeds: Stats answers at once in the middle of one (review T-17).
func TestStatsIsNotBlockedWhileAFileIsEmbedded(t *testing.T) {
	root := t.TempDir()
	emb := &slowEmbedder{started: make(chan struct{}, 1), delay: 150 * time.Millisecond}
	r, err := New(root, emb, "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	writeTestFile(t, root, "notes.txt", strings.Repeat("a line of plain text for the index\n", 300)) // several chunks
	done := make(chan error, 1)
	go func() { _, _, err := r.IngestFile(context.Background(), "notes.txt"); done <- err }()
	<-emb.started
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, _, err := r.Stats(ctx); err != nil {
		t.Fatalf("Stats during an ingest: %v (blocked behind the ingest's transaction)", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if chunks, files, err := r.Stats(context.Background()); err != nil || files != 1 || chunks < 2 {
		t.Fatalf("after the ingest: %d chunks, %d files, %v", chunks, files, err)
	}
}

// A plain-text file past the size cap is skipped, not read whole (review T-17).
func TestAHugeTextFileIsNotIndexed(t *testing.T) {
	root := t.TempDir()
	r, err := New(root, &slowEmbedder{started: make(chan struct{}, 1)}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	writeTestFile(t, root, "big.log", strings.Repeat("x", maxIngestBytes+10))
	if _, _, err := r.IngestFile(context.Background(), "big.log"); err == nil || !strings.Contains(err.Error(), "skip") {
		t.Fatalf("a huge file was indexed: %v", err)
	}
}
