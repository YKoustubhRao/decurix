// Live test: a llama-server keeps the conversation it held across a stop
// and start (its slot saved to disk and restored), and runs with the memory
// settings the plan counted. Runs only with a real server and model.
package engine

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"brainiac/internal/backend"
)

func TestLiveServerKeepsItsPromptAcrossARestart(t *testing.T) {
	bin, model := os.Getenv("BRAINIAC_LIVE_LLAMA"), os.Getenv("BRAINIAC_LIVE_GGUF")
	if bin == "" || model == "" || runtime.GOOS != "linux" {
		t.Skip("set BRAINIAC_LIVE_LLAMA and BRAINIAC_LIVE_GGUF to run a real server on Linux")
	}
	dir := t.TempDir()
	slots := filepath.Join(dir, "slots")
	if err := os.MkdirAll(slots, 0o700); err != nil {
		t.Fatal(err)
	}
	p := &LlamaProc{Bin: bin, Model: model, Ctx: 2048, Threads: max(1, runtime.NumCPU()/2), LogPath: filepath.Join(dir, "llama.log"),
		CacheRAMMiB: 64, Checkpoints: 3, SlotDir: slots, Logf: t.Logf}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := p.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	// A prompt start long enough to be worth keeping, then a short question.
	prefix := strings.Repeat("The ledger lists invoices, payments and credit notes for the month. ", 40)
	ask := func(q string) backend.ChatResult {
		t.Helper()
		r, err := p.Client().Chat(ctx, backend.ChatRequest{MaxTokens: 4, Messages: []backend.Message{{Role: "system", Text: prefix}, {Role: "user", Text: q}}})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	first := ask("How many kinds of entry are listed?")
	t.Logf("first: prompt %d, cached %d", first.PromptTokens, first.CachedTokens)
	p.Stop() // saves the slot
	files, _ := os.ReadDir(slots)
	if len(files) != 1 {
		t.Fatalf("saved slot files: %d, want 1 (log: %s)", len(files), tailOf(p.LogPath))
	}
	if err := p.Start(ctx); err != nil { // restores it
		t.Fatal(err)
	}
	again := ask("Which kind of entry comes first?")
	t.Logf("after restart: prompt %d, cached %d", again.PromptTokens, again.CachedTokens)
	if again.CachedTokens < first.PromptTokens/2 {
		t.Fatalf("after a restart only %d of %d prompt tokens came from the saved state (log: %s)", again.CachedTokens, again.PromptTokens, tailOf(p.LogPath))
	}
	// The build took every flag (the args test pins which ones are passed).
	log, _ := os.ReadFile(p.LogPath)
	if strings.Contains(string(log), "error: unknown argument") || strings.Contains(string(log), "invalid argument") {
		t.Fatalf("the server refused a flag: %s", tailOf(p.LogPath))
	}
}
