// Tests for the flags Brainiac passes llama-server: every setting that
// changes memory or placement is explicit, and only a build that knows a
// flag is given it.
package engine

import (
	"strings"
	"testing"
)

// fakeHelp makes serverHelp answer text for bin without running anything.
func fakeHelp(t *testing.T, bin, text string) {
	t.Helper()
	helpMu.Lock()
	helpCache[bin] = text
	helpMu.Unlock()
	t.Cleanup(func() {
		helpMu.Lock()
		delete(helpCache, bin)
		helpMu.Unlock()
	})
}

// b11541Help lists the flags of the pinned build that Brainiac uses.
const b11541Help = `-m, --model FNAME
--host HOST
--port PORT
-c, --ctx-size N
-t, --threads N
--parallel N
-fit, --fit [on|off]
-ngl, --gpu-layers, --n-gpu-layers N
-cram, --cache-ram N
-ctxcp, --ctx-checkpoints, --swa-checkpoints N
--flash-attn [on|off|auto]
--cache-type-k TYPE
--jinja
--no-webui
--slot-save-path PATH
--spec-default
--embeddings
unix socket: --host /path/x.sock
`

func joined(a []string) string { return " " + strings.Join(a, " ") + " " }

func TestArgsPinWhatThePlanCounted(t *testing.T) {
	fakeHelp(t, "fake-b11541", b11541Help)
	p := &LlamaProc{Bin: "fake-b11541", Model: "m.gguf", Ctx: 8192, Threads: 3, CacheRAMMiB: 125, Checkpoints: 3, SlotDir: "/s"}
	got := joined(p.args("127.0.0.1", 9000))
	for _, want := range []string{" --fit off ", " -ngl 0 ", " --cache-ram 125 ", " --ctx-checkpoints 3 ", " --slot-save-path /s ", " --parallel 1 "} {
		if !strings.Contains(got, want) {
			t.Errorf("args lack %q: %s", want, got)
		}
	}
	if strings.Contains(got, "--spec-default") {
		t.Errorf("speculative decoding is on without being asked for: %s", got)
	}
	// No cache counted means the cache is off, never llama.cpp's 8 GiB default.
	p.CacheRAMMiB = 0
	if got := joined(p.args("127.0.0.1", 9000)); !strings.Contains(got, " --cache-ram 0 ") {
		t.Errorf("a server with no counted cache keeps llama.cpp's default: %s", got)
	}
	p.OnGPU, p.Spec = true, SpecNgram
	got = joined(p.args("127.0.0.1", 9000))
	if !strings.Contains(got, " -ngl 999 ") || !strings.Contains(got, " --spec-default ") {
		t.Errorf("GPU placement or speculation missing: %s", got)
	}
}

func TestArgsForAnEmbeddingServer(t *testing.T) {
	fakeHelp(t, "fake-b11541", b11541Help)
	p := &LlamaProc{Bin: "fake-b11541", Model: "e.gguf", Ctx: 8192, Embedding: true}
	got := joined(p.args("127.0.0.1", 9000))
	if !strings.Contains(got, " --embeddings ") || !strings.Contains(got, " --cache-ram 0 ") || !strings.Contains(got, " --fit off ") {
		t.Errorf("embedding args: %s", got)
	}
}

// An older build that lacks a flag is not given it (it would refuse to start).
func TestArgsLeaveOutFlagsTheBuildLacks(t *testing.T) {
	fakeHelp(t, "fake-old", "-m, --model FNAME\n--host HOST\n--port PORT\n-c, --ctx-size N\n")
	p := &LlamaProc{Bin: "fake-old", Model: "m.gguf", Ctx: 8192, CacheRAMMiB: 125, Checkpoints: 3, SlotDir: "/s", Spec: SpecNgram}
	got := joined(p.args("127.0.0.1", 9000))
	for _, flag := range []string{"--fit", "-ngl", "--cache-ram", "--ctx-checkpoints", "--slot-save-path", "--spec-default"} {
		if strings.Contains(got, " "+flag+" ") {
			t.Errorf("an old build got %s: %s", flag, got)
		}
	}
}
