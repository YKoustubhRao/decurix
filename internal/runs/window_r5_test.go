package runs

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// (a) An attempt on another worker never cuts this one's window: its records and calls are its own.
func TestAnotherWorkersAttemptLeavesTheWindowAlone(t *testing.T) {
	a := tasks.Execution{ID: "a", Attempt: 1, WorkerID: "eve", StartedAt: t0, EndedAt: t0.Add(time.Minute)}
	other := tasks.Execution{ID: "o", Attempt: 2, WorkerID: "olga", StartedAt: t0.Add(70 * time.Second)}
	w := windowOf(a, []tasks.Execution{a, other})
	if !w.to.Equal(a.EndedAt.Add(callWindowAfter)) {
		t.Fatalf("another worker's attempt cut the window to %v", w.to)
	}
}

// (b) An event's time is the recording's start plus its "t", not its whole-second "at": an event 0.6 s into a
// back-to-back retry belongs to the retry, though its "at" falls in the attempt before.
func TestAnEventsTimeIsToTheMillisecond(t *testing.T) {
	start := float64(t0.Unix()) + 0.5 // the recording started half a second into t0's second
	tt := 0.6                         // 1.1 s after t0's second began
	ev := recEvent{T: &tt, At: t0.Add(time.Second).Format("2006-01-02T15:04:05-0700")}
	got := eventTime(ev, recMeta{StartedAt: start})
	if want := t0.Add(1100 * time.Millisecond); got.Sub(want).Abs() > time.Millisecond {
		t.Fatalf("eventTime = %v, want %v", got, want)
	}
	first := tasks.Execution{ID: "a", Attempt: 1, WorkerID: "eve", StartedAt: t0.Add(-time.Minute), EndedAt: t0}
	retry := tasks.Execution{ID: "b", Attempt: 2, WorkerID: "eve", StartedAt: t0.Add(1050 * time.Millisecond)}
	all := []tasks.Execution{first, retry}
	if !windowOf(retry, all).holds(got) || windowOf(first, all).holds(got) {
		t.Fatalf("the event at %v went to the wrong attempt", got)
	}
	// With no "t" (or no start), "at" is used.
	if got := eventTime(recEvent{At: ev.At}, recMeta{StartedAt: start}); !got.Equal(t0.Add(time.Second)) {
		t.Fatalf("eventTime without t = %v", got)
	}
}

// racingHome starts a second pull of the same run from inside the first pull's EXPORTED write, and waits a little
// for the second's write to finish before writing its own.
type racingHome struct {
	*fakeHome
	mu       sync.Mutex
	writes   int
	second   func()
	secondOK chan struct{}
	exported string
}

func (r *racingHome) WriteTar(ctx context.Context, id, dir string, archive io.Reader) error {
	r.mu.Lock()
	r.writes++
	n := r.writes
	r.mu.Unlock()
	if n == 1 {
		go r.second()
		select { // without the store's lock, the second pull saves and writes in here
		case <-r.secondOK:
		case <-time.After(300 * time.Millisecond):
		}
	}
	if err := r.fakeHome.WriteTar(ctx, id, dir, archive); err != nil {
		return err
	}
	r.mu.Lock()
	r.exported = string(r.written[".decurix/runs/t_1/EXPORTED"])
	r.mu.Unlock()
	if n == 2 {
		close(r.secondOK)
	}
	return nil
}

// (c) Two pulls of one execution at once: EXPORTED ends with the manifest that is stored, never an older one.
func TestConcurrentPullsLeaveTheStoredManifestInExported(t *testing.T) {
	h := &racingHome{fakeHome: fixtureHome(), secondOK: make(chan struct{})}
	p, _ := newPuller(t, h.fakeHome)
	p.Home = h
	done := make(chan struct{})
	h.second = func() {
		defer close(done)
		h.fakeHome.mu.Lock()
		h.fakeHome.files[".decurix/ai/traces/t_1.jsonl"] = []byte(`{"kind":"call","later":true}` + "\n")
		h.fakeHome.mu.Unlock()
		if _, err := p.Pull(context.Background(), ref1); err != nil {
			t.Error(err)
		}
	}
	if _, err := p.Pull(context.Background(), ref1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the second pull never finished")
	}
	stored, _, _ := p.Store.Bundle("e_1")
	h.mu.Lock()
	exported := h.exported
	h.mu.Unlock()
	if !strings.Contains(exported, stored.ManifestSHA) {
		t.Fatalf("EXPORTED %q does not name the stored manifest %s", exported, stored.ManifestSHA)
	}
}
