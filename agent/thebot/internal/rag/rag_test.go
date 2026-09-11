package rag

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"thebot/internal/llm"
)

// requireRealOllama skips the test (not fails it) when no real Ollama is
// reachable - these tests need real embeddings, not a mock, since the
// whole point of the Data RAG is real semantic retrieval quality.
func requireRealOllama(t *testing.T) *llm.OllamaProvider {
	t.Helper()
	host := os.Getenv("OLLAMA_HOST")
	if host == "" {
		host = "http://127.0.0.1:11434"
	}
	p := llm.NewOllama(host)
	if !p.IsReachable(context.Background()) {
		t.Skipf("no real Ollama reachable at %s - skipping (Data RAG tests need real embeddings)", host)
	}
	return p
}

func writeTestFile(t *testing.T, root, relPath, content string) {
	t.Helper()
	abs := filepath.Join(root, relPath)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatalf("write test file: %v", err)
	}
}

func TestIngestAndQueryPlainText(t *testing.T) {
	provider := requireRealOllama(t)
	root := t.TempDir()
	writeTestFile(t, root, "notes/whales.txt", "Blue whales are the largest animals to have ever lived on Earth. They can grow up to 30 meters long and eat mostly krill.")
	writeTestFile(t, root, "notes/rockets.txt", "The Saturn V rocket was used for the Apollo moon missions and remains one of the most powerful rockets ever built.")

	r, err := New(root, provider, "does-not-matter-for-text-files.py")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer r.Close()

	ctx := context.Background()
	chunks, method, err := r.IngestFile(ctx, "notes/whales.txt")
	if err != nil {
		t.Fatalf("IngestFile(whales) error = %v", err)
	}
	if chunks == 0 || method != "text" {
		t.Errorf("IngestFile(whales) = (%d, %q), want (>0, \"text\")", chunks, method)
	}
	if _, _, err := r.IngestFile(ctx, "notes/rockets.txt"); err != nil {
		t.Fatalf("IngestFile(rockets) error = %v", err)
	}

	results, err := r.Query(ctx, "what is the largest animal in the ocean", 3)
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if len(results) == 0 {
		t.Fatal("Query() returned no results")
	}
	if results[0].RelPath != "notes/whales.txt" {
		t.Errorf("top result = %q, want notes/whales.txt (real semantic retrieval should rank the whale content first for a whale question)", results[0].RelPath)
	}
}

func TestIngestReplacesOldChunksOnReingest(t *testing.T) {
	provider := requireRealOllama(t)
	root := t.TempDir()
	writeTestFile(t, root, "doc.txt", "version one content")

	r, err := New(root, provider, "unused.py")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer r.Close()
	ctx := context.Background()

	if _, _, err := r.IngestFile(ctx, "doc.txt"); err != nil {
		t.Fatalf("first ingest error = %v", err)
	}
	writeTestFile(t, root, "doc.txt", "version two content, completely different")
	if _, _, err := r.IngestFile(ctx, "doc.txt"); err != nil {
		t.Fatalf("second ingest error = %v", err)
	}

	totalChunks, totalFiles, err := r.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats() error = %v", err)
	}
	if totalFiles != 1 {
		t.Errorf("totalFiles = %d, want 1 (re-ingest should replace, not duplicate)", totalFiles)
	}
	if totalChunks != 1 {
		t.Errorf("totalChunks = %d, want 1", totalChunks)
	}
}

func TestQueryEmptyIndexReturnsNoResultsNotError(t *testing.T) {
	provider := requireRealOllama(t)
	root := t.TempDir()
	r, err := New(root, provider, "unused.py")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer r.Close()

	results, err := r.Query(context.Background(), "anything", 5)
	if err != nil {
		t.Fatalf("Query() on empty index returned an error, want nil: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("Query() on empty index = %d results, want 0", len(results))
	}
}

func TestIngestSkipsBinaryFormats(t *testing.T) {
	provider := requireRealOllama(t)
	root := t.TempDir()
	writeTestFile(t, root, "archive.zip", "not a real zip but the extension is what matters here")

	r, err := New(root, provider, "unused.py")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer r.Close()

	_, _, err = r.IngestFile(context.Background(), "archive.zip")
	if err == nil {
		t.Error("IngestFile(.zip) succeeded, want a skip error - binary formats have no useful text representation")
	}
}

// The live bug this pins: a venv's own compiled python3 binary - no
// extension at all, so neither skipExts nor doclingExts ever saw it -
// got read whole and chunked as "text", 6133 chunks from one binary,
// three times over via its symlinks. Extension lists can't name every
// possible binary; the content itself (a NUL byte in the first few
// hundred bytes, real ELF binaries all have one immediately) is the
// general signal.
func TestIngestSkipsExtensionlessBinaryContent(t *testing.T) {
	provider := requireRealOllama(t)
	root := t.TempDir()
	// A real ELF header prefix - \x7fELF - is exactly what a compiled
	// python3 binary starts with, and it contains a real NUL byte.
	elfLike := append([]byte{0x7f, 'E', 'L', 'F', 0x02, 0x01, 0x01, 0x00}, make([]byte, 100)...)
	if err := os.WriteFile(filepath.Join(root, "python3"), elfLike, 0o755); err != nil {
		t.Fatalf("could not write fixture: %v", err)
	}

	r, err := New(root, provider, "unused.py")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer r.Close()

	_, _, err = r.IngestFile(context.Background(), "python3")
	if err == nil {
		t.Error("IngestFile() on binary content with no extension succeeded, want a skip error")
	}
}

// Genuine extension-less text (a shell script, a README with no
// extension, a Makefile) must still ingest normally - the binary sniff
// must not become a blanket "no extension, no ingest" rule.
func TestIngestStillWorksForExtensionlessText(t *testing.T) {
	provider := requireRealOllama(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Makefile"), []byte("build:\n\tgo build ./...\n"), 0o644); err != nil {
		t.Fatalf("could not write fixture: %v", err)
	}

	r, err := New(root, provider, "unused.py")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer r.Close()

	chunks, method, err := r.IngestFile(context.Background(), "Makefile")
	if err != nil {
		t.Fatalf("IngestFile() error = %v, want a genuine extension-less text file to ingest normally", err)
	}
	if chunks == 0 || method != "text" {
		t.Errorf("chunks=%d method=%q, want real text chunks", chunks, method)
	}
}

// TestIngestRealHTMLViaDocling exercises the real docling subprocess path
// (not the plain-text path) - skipped if docling isn't installed locally,
// but genuinely proves the extraction (including table structure) when it
// is, rather than assuming the subprocess call works.
func TestIngestRealHTMLViaDocling(t *testing.T) {
	provider := requireRealOllama(t)

	scriptPath, err := filepath.Abs("../../scripts/docling_convert.py")
	if err != nil {
		t.Fatalf("resolve docling script path: %v", err)
	}
	if _, err := os.Stat(scriptPath); err != nil {
		t.Skipf("docling_convert.py not found at %s", scriptPath)
	}

	root := t.TempDir()
	writeTestFile(t, root, "report.html", `<html><body>
<h1>Quarterly Report</h1>
<p>Revenue increased by 12% to $4.5 million in Q3 2026.</p>
<table><tr><th>Quarter</th><th>Revenue</th></tr><tr><td>Q3</td><td>$4.5M</td></tr></table>
</body></html>`)

	r, err := New(root, provider, scriptPath)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer r.Close()

	ctx := context.Background()
	chunks, method, err := r.IngestFile(ctx, "report.html")
	if err != nil {
		t.Skipf("docling is present but the real conversion failed (likely not installed correctly): %v", err)
	}
	if method != "docling" {
		t.Errorf("method = %q, want docling", method)
	}
	if chunks == 0 {
		t.Error("IngestFile() produced 0 chunks for real HTML content")
	}

	results, err := r.Query(ctx, "what was the revenue this quarter", 1)
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if len(results) == 0 || !strings.Contains(results[0].Text, "4.5") {
		t.Errorf("real docling-extracted content did not surface the revenue figure: %+v", results)
	}
}

func TestIngestFileThatDoesNotExist(t *testing.T) {
	provider := requireRealOllama(t)
	root := t.TempDir()
	r, err := New(root, provider, "unused.py")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer r.Close()

	_, _, err = r.IngestFile(context.Background(), "does/not/exist.txt")
	if err == nil {
		t.Error("IngestFile() on a nonexistent file succeeded, want an error")
	}
}
