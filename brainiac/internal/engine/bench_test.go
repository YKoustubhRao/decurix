// Tests for brainiac bench's measurement, against the fake llama-server in
// its "timed" mode (100 prompt and 10 answer tokens a second, a prompt cache).
package engine

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	"brainiac/internal/catalog"
	"brainiac/internal/install"
	"brainiac/internal/selector"
)

func timedLock(t *testing.T) *install.Lock {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(t.TempDir(), "m.gguf")
	if err := os.WriteFile(model, []byte("timed"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := catalog.Source{Kind: catalog.SourceHF, Runtime: catalog.RuntimeLlamaCpp, Repo: "fake/fake", Quant: "Q4"}
	pick := selector.Pick{Role: catalog.RoleBrain, Model: "fake", Pool: "host", MemGB: 0.1, Source: src}
	return &install.Lock{LlamaServer: self, Plan: &selector.Plan{Picks: []selector.Pick{pick}},
		Units: map[string]install.Unit{install.UnitKey("fake", src): {Files: map[string]string{"model": model}}}}
}

func near(a, b float64) bool { return math.Abs(a-b) <= 0.05*math.Max(1, math.Abs(b)) }

func TestBenchMeasuresReadingWritingAndTheCache(t *testing.T) {
	cat, _ := catalog.Default()
	res, err := MeasureSpeed(context.Background(), timedLock(t), cat, SpeedOptions{Threads: []int{1, 2}, LongTokens: 2000, NoSandbox: true, TempDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("%d results", len(res))
	}
	r := res[0]
	if !near(r.PrefillShortTPS, 100) || !near(r.PrefillLongTPS, 100) || !near(r.DecodeTPS, 10) || !near(r.DecodeLongTPS, 10) {
		t.Fatalf("speeds %+v", r)
	}
	if r.LongTokens < 1800 || r.LongTokens > 2300 {
		t.Fatalf("long prompt %d tokens, asked for about 2000", r.LongTokens)
	}
	// Cold: the whole long prompt at 100 tok/s plus 64 tokens at 10 tok/s.
	if want := float64(r.LongTokens)/100 + 6.4; !near(r.ColdTurnSec, want) {
		t.Fatalf("cold turn %.1f s, want %.1f", r.ColdTurnSec, want)
	}
	if r.WarmCachedShare < 0.95 || r.WarmTurnSec >= r.ColdTurnSec/3 {
		t.Fatalf("warm turn %.1f s with %.0f%% cached, cold %.1f s", r.WarmTurnSec, 100*r.WarmCachedShare, r.ColdTurnSec)
	}
	if !near(r.Sustained, 1) || len(r.ThreadsTried) != 2 || r.Threads == 0 || r.File == "" || r.Build == "" {
		t.Fatalf("result %+v", r)
	}
}

// A local answer's trace carries llama-server's timings, and status shows
// the model's cache hit rate and speeds.
func TestTimingsReachTheTraceAndStatus(t *testing.T) {
	e, _ := llamaEngine(t, "timed")
	ask := func(q string) *Response {
		t.Helper()
		res, err := e.Chat(context.Background(), Request{MaxTokens: 8, Messages: []InMessage{
			{Role: "system", Parts: []Part{{Kind: "text", Text: benchText(9, 6000)}}},
			{Role: "user", Parts: []Part{{Kind: "text", Text: q}}}}})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	ask("first")
	res := ask("second")
	var tok *struct{ prompt, cached int }
	for _, a := range res.Trace.Atoms {
		if a.Op == "brain" && a.Tokens != nil {
			if a.Tokens.PromptMs <= 0 || a.Tokens.GenMs <= 0 || a.Tokens.Cached <= 0 {
				t.Fatalf("trace tokens %+v", a.Tokens)
			}
			tok = &struct{ prompt, cached int }{a.Tokens.Prompt, a.Tokens.Cached}
		}
	}
	if tok == nil {
		t.Fatal("no brain atom with tokens")
	}
	st := e.Status().Speed["fake"]
	if st.Calls != 2 || !near(st.PrefillTPS, 100) || !near(st.DecodeTPS, 10) || st.CacheHitRate <= 0.4 {
		t.Fatalf("status speed %+v", st)
	}
}
