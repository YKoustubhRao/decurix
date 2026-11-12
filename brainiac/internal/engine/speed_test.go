// Tests for what local calls measured: the prompt-cache hit rate, speeds,
// and the warning when long prompts stop coming from the cache.
package engine

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"brainiac/internal/backend"
)

func TestSpeedSummaryJudgesTheCacheOnLongPrompts(t *testing.T) {
	l := newLiveSpeed()
	var buf bytes.Buffer
	lg := log.New(&buf, "", 0)
	// Short calls do not count toward the hit rate.
	for i := 0; i < 10; i++ {
		l.record("m", backend.Timings{PromptN: 50, PromptMs: 1000, PredictedN: 10, PredictedMs: 1000}, lg)
	}
	st, _ := l.get("m")
	if st.CacheHitRate != -1 || st.Warning != "" {
		t.Fatalf("short prompts judged: %+v", st)
	}
	if st.PrefillTPS != 50 || st.DecodeTPS != 10 {
		t.Fatalf("speeds %+v, want 50 and 10 tok/s", st)
	}
	// Long prompts mostly from the cache: no warning.
	for i := 0; i < 6; i++ {
		l.record("m", backend.Timings{CacheN: 5000, PromptN: 200, PromptMs: 4000, PredictedN: 20, PredictedMs: 2000}, lg)
	}
	if st, _ := l.get("m"); st.CacheHitRate < 0.9 || st.Warning != "" {
		t.Fatalf("a working cache judged: %+v", st)
	}
	// Then they are all read from their start: a warning, logged once.
	for i := 0; i < speedWindow; i++ {
		l.record("m", backend.Timings{CacheN: 0, PromptN: 5000, PromptMs: 100000, PredictedN: 20, PredictedMs: 2000}, lg)
	}
	st, _ = l.get("m")
	if st.CacheHitRate != 0 || st.Warning == "" {
		t.Fatalf("a missing cache not flagged: %+v", st)
	}
	if n := strings.Count(buf.String(), "prompt cache"); n != 1 {
		t.Fatalf("warning logged %d times, want once:\n%s", n, buf.String())
	}
	if snap := l.snapshot(); snap["m"].Calls != speedWindow {
		t.Fatalf("snapshot %+v keeps more than the window", snap)
	}
}

func TestSpeedSummaryCountsDrafts(t *testing.T) {
	l := newLiveSpeed()
	l.record("m", backend.Timings{PredictedN: 100, PredictedMs: 5000, DraftN: 40, DraftAccepted: 30}, nil)
	if st, _ := l.get("m"); st.DraftAcceptRate != 0.75 {
		t.Fatalf("draft acceptance %+v, want 0.75", st)
	}
}
