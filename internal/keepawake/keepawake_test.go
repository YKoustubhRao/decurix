package keepawake

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeHolder struct {
	mu     sync.Mutex
	on     bool
	closed bool
	calls  []bool
}

func (f *fakeHolder) set(on bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.on = on
	f.calls = append(f.calls, on)

	return nil
}

func (f *fakeHolder) alive() bool { return true }

func (f *fakeHolder) close() {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.closed = true
}

func (f *fakeHolder) state() (bool, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.on, f.closed
}

func TestKeeperHoldsOnlyWhileWantedAndAlwaysReleases(t *testing.T) {
	t.Setenv("DECURIX_KEEP_AWAKE", "")

	fake := &fakeHolder{}
	platformHolder = func() (holder, string) { return fake, "fake" }
	checkEvery = 5 * time.Millisecond
	t.Cleanup(func() { platformHolder = newHolder; checkEvery = 30 * time.Second })

	var mu sync.Mutex
	reasons := []string{}

	k := &Keeper{Wanted: func() []string {
		mu.Lock()
		defer mu.Unlock()
		return reasons
	}}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { k.Run(ctx); close(done) }()

	waitFor := func(want bool) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if on, _ := fake.state(); on == want && k.State().Held == want {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
		t.Fatalf("held = %v, want %v", !want, want)
	}

	time.Sleep(20 * time.Millisecond)
	if on, _ := fake.state(); on {
		t.Fatal("held with nothing wanting it")
	}

	mu.Lock()
	reasons = []string{"mailboxes are watched"}
	mu.Unlock()
	waitFor(true)

	if st := k.State(); !st.Wanted || !st.Held {
		t.Errorf("state = %+v", st)
	}

	mu.Lock()
	reasons = nil
	mu.Unlock()
	waitFor(false)

	mu.Lock()
	reasons = []string{"schedules are waiting to run"}
	mu.Unlock()
	waitFor(true)

	cancel()
	<-done

	if on, closed := fake.state(); on || !closed {
		t.Errorf("after stopping: held %v, closed %v", on, closed)
	}
}

func TestKeepAwakeOffNeverHolds(t *testing.T) {
	t.Setenv("DECURIX_KEEP_AWAKE", "0")

	fake := &fakeHolder{}
	platformHolder = func() (holder, string) { return fake, "fake" }
	t.Cleanup(func() { platformHolder = newHolder })

	k := &Keeper{Wanted: func() []string { return []string{"mailboxes are watched"} }}
	k.Run(context.Background()) // returns at once when off

	if len(fake.calls) != 0 {
		t.Errorf("set was called: %v", fake.calls)
	}

	if st := k.State(); st.Wanted {
		t.Errorf("state = %+v", st)
	}
}
