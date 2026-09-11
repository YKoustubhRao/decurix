package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"thebot/internal/llm"
	"thebot/internal/rag"
)

func realEmbedderOrSkip(t *testing.T) *llm.OllamaProvider {
	t.Helper()
	host := os.Getenv("OLLAMA_HOST")
	if host == "" {
		host = "http://127.0.0.1:11434"
	}
	p := llm.NewOllama(host)
	if !p.IsReachable(context.Background()) {
		t.Skipf("no real Ollama at %s - rag-query tests need real embeddings", host)
	}
	return p
}

// The live regression this pins: asked which region grew fastest in a
// real 10-K, the answer was wrong - "Americas at 7%" instead of the real
// "Japan at 15%" - because the retrieved chunk was truncated to 800
// characters before ever reaching the model, and Japan's row sat at
// character ~950. The model reasoned correctly from what it was shown;
// what it was shown had been mutilated first. A retrieved chunk's full
// text must reach the synthesis prompt, not a truncated prefix of it.
func TestRAGQuerySendsFullChunkTextNotTruncated(t *testing.T) {
	embedder := realEmbedderOrSkip(t)
	dir := t.TempDir()
	r, err := rag.New(dir, embedder, "")
	if err != nil {
		t.Fatalf("rag.New() error = %v", err)
	}
	defer r.Close()

	// A single chunk (under the 1500-rune chunker cap) with the marker
	// data placed well past the old 800-char truncation point - exactly
	// where "Japan | 15%" sat in the real filing.
	filler := strings.Repeat("Americas region net sales performance narrative text. ", 15) // ~850 chars
	content := filler + "\nJapan segment grew 15 percent year over year, the fastest of any region.\n"
	if len(filler) < 800 {
		t.Fatalf("test setup: filler is only %d chars, want > 800 to actually exercise the old cutoff", len(filler))
	}

	path := filepath.Join(dir, "filing.txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("could not write fixture file: %v", err)
	}
	if _, _, err := r.IngestFile(context.Background(), "filing.txt"); err != nil {
		t.Fatalf("IngestFile() error = %v", err)
	}

	stub := &stubChatProvider{response: "Japan grew fastest at 15%."}
	rc := RunContext{
		Ctx: context.Background(), Provider: stub, Model: "test-model",
		RAG: r, OnLog: func(string) {},
	}

	res, err := NewRAGQuery().Run(rc, Subtask{Step: "which region grew fastest?", Type: "rag-query"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !res.Verified {
		t.Fatalf("Run() not verified: %s", res.Output)
	}

	if len(stub.gotMessages) == 0 {
		t.Fatal("no message reached the model")
	}
	sent := stub.gotMessages[len(stub.gotMessages)-1].Content
	if !strings.Contains(sent, "Japan segment grew 15 percent") {
		t.Errorf("the marker text past the old 800-char cutoff never reached the model.\nprompt was:\n%s", sent)
	}
}
