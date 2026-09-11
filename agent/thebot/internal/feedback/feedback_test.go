package feedback

import (
	"context"
	"os"
	"testing"

	"thebot/internal/llm"
)

func requireRealOllama(t *testing.T) *llm.OllamaProvider {
	t.Helper()
	host := os.Getenv("OLLAMA_HOST")
	if host == "" {
		host = "http://127.0.0.1:11434"
	}
	p := llm.NewOllama(host)
	if !p.IsReachable(context.Background()) {
		t.Skipf("no real Ollama reachable at %s - skipping (feedback tests need real embeddings)", host)
	}
	return p
}

func TestConsultEmptyStoreReturnsNilNotError(t *testing.T) {
	provider := requireRealOllama(t)
	s, err := New(t.TempDir(), provider)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer s.Close()

	entries, err := s.ConsultNegative(context.Background(), "anything", 5)
	if err != nil {
		t.Fatalf("ConsultNegative() on empty store returned an error, want nil: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("ConsultNegative() on empty store = %d entries, want 0", len(entries))
	}
}

func TestRecordAndConsultFindsRelevantEntry(t *testing.T) {
	provider := requireRealOllama(t)
	s, err := New(t.TempDir(), provider)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	if err := s.RecordNegative(ctx, "parsing PDF files", "cannot reliably extract tables from scanned PDFs", "user"); err != nil {
		t.Fatalf("RecordNegative() error = %v", err)
	}
	if err := s.RecordNegative(ctx, "writing Go code", "sometimes forgets to handle nil pointer cases", "self"); err != nil {
		t.Fatalf("RecordNegative() error = %v", err)
	}

	entries, err := s.ConsultNegative(ctx, "extract data from a scanned PDF document", 3)
	if err != nil {
		t.Fatalf("ConsultNegative() error = %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("ConsultNegative() returned no entries for a clearly related query")
	}
	if entries[0].Task != "parsing PDF files" {
		t.Errorf("top match = %q, want %q (real semantic retrieval should rank the PDF-related entry first)", entries[0].Task, "parsing PDF files")
	}
	if entries[0].Source != "user" {
		t.Errorf("top match Source = %q, want %q", entries[0].Source, "user")
	}
}

func TestCount(t *testing.T) {
	provider := requireRealOllama(t)
	s, err := New(t.TempDir(), provider)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	n, err := s.Count(ctx)
	if err != nil {
		t.Fatalf("Count() error = %v", err)
	}
	if n != 0 {
		t.Errorf("Count() on fresh store = %d, want 0", n)
	}

	_ = s.RecordNegative(ctx, "task", "detail", "self")
	n, err = s.Count(ctx)
	if err != nil {
		t.Fatalf("Count() error = %v", err)
	}
	if n != 1 {
		t.Errorf("Count() after recording 1 entry = %d, want 1", n)
	}
}
