package ollama

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// resetLive empties the measurement CurrentStatus answers from, for a test.
func resetLive(t *testing.T) {
	t.Helper()
	live.mu.Lock()
	live.at, live.url, live.refreshing = time.Time{}, "", false
	live.sel, live.res, live.mode, live.err = Selection{}, Resources{}, "", nil
	live.mu.Unlock()
}

// The model status answers from what is known, at once, while a measurement waits on a slow host (the dashboard's
// /api/model took 30 s on every page load, resource bench 2026-11-05): first "measuring", then the measurement once
// it is in; one measurement at a time however many ask; and a stale one is answered while the next is made.
func TestTheModelStatusNeverWaitsOnAMeasurement(t *testing.T) {
	t.Setenv("OLLAMA_URL", BundledURL)
	t.Setenv("OLLAMA_MODEL", "llama3.2:1b")
	resetLive(t)
	release := make(chan struct{})
	refreshed := make(chan struct{}, 4)
	var calls atomic.Int32
	savedEval, savedDone := liveEvaluate, liveRefreshed
	liveEvaluate = func(ctx context.Context, _ string) (Selection, Resources, string, error) {
		calls.Add(1)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return Selection{Model: Candidate{Name: "llama3.2:1b"}, Reason: "measured"}, Resources{Cores: 8}, "auto", nil
	}
	liveRefreshed = func() { refreshed <- struct{}{} }
	t.Cleanup(func() { liveEvaluate, liveRefreshed = savedEval, savedDone; resetLive(t) })

	answered := make(chan Status, 1)
	go func() { answered <- CurrentStatus(context.Background()) }()
	var st Status
	// The bound only turns a hang into a failure: the measurement is held until release, so an answer that came
	// at all came without it.
	select {
	case st = <-answered:
	case <-time.After(time.Minute):
		t.Fatal("the model status waited on the measurement")
	}
	if st.Reason != "Measuring this machine…" || st.Mode != "pinned" {
		t.Fatalf("first answer %+v", st)
	}
	for i := 0; i < 5; i++ {
		_ = CurrentStatus(context.Background())
	}
	close(release)
	<-refreshed
	if n := calls.Load(); n != 1 {
		t.Fatalf("%d measurements for six asks, want one at a time", n)
	}
	if st := CurrentStatus(context.Background()); st.Resources.Cores != 8 {
		t.Fatalf("after the measurement %+v", st)
	}

	// Stale: answered from it at once, and one new measurement made behind it.
	live.mu.Lock()
	live.at = time.Now().Add(-time.Hour)
	live.mu.Unlock()
	if st := CurrentStatus(context.Background()); st.Resources.Cores != 8 {
		t.Fatalf("a stale measurement was not answered from: %+v", st)
	}
	<-refreshed
	if n := calls.Load(); n != 2 {
		t.Fatalf("%d measurements, want a second after the first went stale", n)
	}
}
