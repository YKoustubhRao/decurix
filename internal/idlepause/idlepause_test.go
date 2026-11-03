package idlepause

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
)

// fakeDesktops is a DesktopLister a test can script without Docker. sweep
// now checks multiple desktops concurrently (see sweepConcurrency), so
// Stop/List/LastViewed all need to be safe for concurrent use, the same as
// the real *desktop.Manager they stand in for.
type fakeDesktops struct {
	list []desktop.Desktop

	mu      sync.Mutex
	stopped []string
	paused  []string
	viewed  map[string]time.Time
}

func (f *fakeDesktops) List(context.Context) ([]desktop.Desktop, error) { return f.list, nil }

func (f *fakeDesktops) Stop(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.stopped = append(f.stopped, id)
	return nil
}

func (f *fakeDesktops) Pause(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.paused = append(f.paused, id)
	return nil
}

func (f *fakeDesktops) LastViewed(id string) (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	t, ok := f.viewed[id]
	return t, ok
}

// fakeTasks is an ActivityStore a test can script.
type fakeTasks struct {
	busy         map[string]string
	activity     map[string]time.Time
	activityErrs map[string]error
}

func (f *fakeTasks) Busy(workerID string) (string, error) { return f.busy[workerID], nil }

func (f *fakeTasks) LastActivity(workerID string) (time.Time, bool, error) {
	if err, ok := f.activityErrs[workerID]; ok {
		return time.Time{}, false, err
	}

	t, ok := f.activity[workerID]
	return t, ok, nil
}

const day = 24 * time.Hour

func TestBusyDesktopIsNeverStopped(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	d := desktop.Desktop{ID: "bordu", Status: "running", StartedAt: now.Add(-day).Format(time.RFC3339Nano)}
	desktops := &fakeDesktops{list: []desktop.Desktop{d}}
	tasks := &fakeTasks{busy: map[string]string{"bordu": "a task is still running"}}

	c := &Checker{Desktops: desktops, Tasks: tasks, Threshold: time.Hour, Action: "stop", now: func() time.Time { return now }}
	c.sweep(context.Background())

	if len(desktops.stopped) != 0 {
		t.Fatalf("stopped a busy desktop: %v", desktops.stopped)
	}
}

func TestIdlePastThresholdIsStopped(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	d := desktop.Desktop{ID: "bordu", Status: "running", StartedAt: now.Add(-day).Format(time.RFC3339Nano)}
	desktops := &fakeDesktops{list: []desktop.Desktop{d}}
	tasks := &fakeTasks{activity: map[string]time.Time{"bordu": now.Add(-2 * time.Hour)}}

	c := &Checker{Desktops: desktops, Tasks: tasks, Threshold: time.Hour, Action: "stop", now: func() time.Time { return now }}
	c.sweep(context.Background())

	if len(desktops.stopped) != 1 || desktops.stopped[0] != "bordu" {
		t.Fatalf("stopped = %v, want [bordu]", desktops.stopped)
	}
}

func TestLastActivityErrorFailsSafeRatherThanStopping(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	// StartedAt alone is already past Threshold - the old, buggy behavior
	// silently fell back to it whenever LastActivity errored, and would
	// have stopped this desktop even though its real last-activity signal
	// (unreachable here) could easily have been recent.
	d := desktop.Desktop{ID: "bordu", Status: "running", StartedAt: now.Add(-day).Format(time.RFC3339Nano)}
	desktops := &fakeDesktops{list: []desktop.Desktop{d}}
	tasks := &fakeTasks{activityErrs: map[string]error{"bordu": errors.New("database is locked")}}

	c := &Checker{Desktops: desktops, Tasks: tasks, Threshold: time.Hour, Action: "stop", now: func() time.Time { return now }}
	c.sweep(context.Background())

	if len(desktops.stopped) != 0 {
		t.Fatalf("stopped a desktop despite a LastActivity error: %v", desktops.stopped)
	}
}

func TestNotYetIdleEnoughIsLeftAlone(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	d := desktop.Desktop{ID: "bordu", Status: "running", StartedAt: now.Add(-day).Format(time.RFC3339Nano)}
	desktops := &fakeDesktops{list: []desktop.Desktop{d}}
	tasks := &fakeTasks{activity: map[string]time.Time{"bordu": now.Add(-30 * time.Minute)}}

	c := &Checker{Desktops: desktops, Tasks: tasks, Threshold: time.Hour, Action: "stop", now: func() time.Time { return now }}
	c.sweep(context.Background())

	if len(desktops.stopped) != 0 {
		t.Fatalf("stopped a desktop that has not been idle long enough: %v", desktops.stopped)
	}
}

func TestNoStartTimeSignalIsLeftAlone(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	// The Docker zero value for "never started".
	d := desktop.Desktop{ID: "bordu", Status: "running", StartedAt: "0001-01-01T00:00:00Z"}
	desktops := &fakeDesktops{list: []desktop.Desktop{d}}
	tasks := &fakeTasks{}

	c := &Checker{Desktops: desktops, Tasks: tasks, Threshold: time.Minute, Action: "stop", now: func() time.Time { return now }}
	c.sweep(context.Background())

	if len(desktops.stopped) != 0 {
		t.Fatalf("stopped a desktop with no start-time signal: %v", desktops.stopped)
	}
}

func TestRecentlyViewedDesktopIsNotStoppedEvenWithOldTaskActivity(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	d := desktop.Desktop{ID: "bordu", Status: "running", StartedAt: now.Add(-day).Format(time.RFC3339Nano)}
	desktops := &fakeDesktops{
		list:   []desktop.Desktop{d},
		viewed: map[string]time.Time{"bordu": now.Add(-time.Minute)},
	}
	tasks := &fakeTasks{activity: map[string]time.Time{"bordu": now.Add(-3 * day)}}

	c := &Checker{Desktops: desktops, Tasks: tasks, Threshold: time.Hour, Action: "stop", now: func() time.Time { return now }}
	c.sweep(context.Background())

	if len(desktops.stopped) != 0 {
		t.Fatalf("stopped a desktop someone just looked at: %v", desktops.stopped)
	}
}

func TestStoppedDesktopIsNeverConsidered(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	d := desktop.Desktop{ID: "bordu", Status: "stopped"}
	desktops := &fakeDesktops{list: []desktop.Desktop{d}}
	tasks := &fakeTasks{activity: map[string]time.Time{"bordu": now.Add(-3 * day)}}

	c := &Checker{Desktops: desktops, Tasks: tasks, Threshold: time.Minute, Action: "stop", now: func() time.Time { return now }}
	c.sweep(context.Background())

	if len(desktops.stopped) != 0 {
		t.Fatalf("attempted to stop an already-stopped desktop: %v", desktops.stopped)
	}
}

func TestZeroThresholdDisablesRunEntirely(t *testing.T) {
	desktops := &fakeDesktops{list: []desktop.Desktop{{ID: "bordu", Status: "running"}}}
	tasks := &fakeTasks{}

	c := &Checker{Desktops: desktops, Tasks: tasks}

	done := make(chan struct{})
	go func() {
		c.Run(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run with no threshold should return immediately instead of looping")
	}
}

func TestSweepChecksManyDesktopsConcurrentlyAndStopsAllThatAreIdle(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	var list []desktop.Desktop
	activity := map[string]time.Time{}

	// More than sweepConcurrency, so this also exercises the bound: not
	// every desktop can run at once, but every one must still be checked.
	const n = sweepConcurrency*2 + 3

	var want []string
	for i := 0; i < n; i++ {
		id := "d" + string(rune('a'+i))
		list = append(list, desktop.Desktop{ID: id, Status: "running", StartedAt: now.Add(-24 * time.Hour).Format(time.RFC3339Nano)})
		activity[id] = now.Add(-2 * time.Hour) // well past the threshold
		want = append(want, id)
	}

	desktops := &fakeDesktops{list: list}
	tasks := &fakeTasks{activity: activity}

	c := &Checker{Desktops: desktops, Tasks: tasks, Threshold: time.Hour, Action: "stop", now: func() time.Time { return now }}
	c.sweep(context.Background())

	got := append([]string(nil), desktops.stopped...)
	sort.Strings(got)
	sort.Strings(want)

	if len(got) != len(want) {
		t.Fatalf("stopped %d desktops, want %d (got: %v)", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("stopped = %v, want %v", got, want)
		}
	}
}

func TestIdlePastThresholdIsPaused(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	d := desktop.Desktop{ID: "bordu", Status: "running", StartedAt: now.Add(-day).Format(time.RFC3339Nano)}
	desktops := &fakeDesktops{list: []desktop.Desktop{d}}
	tasks := &fakeTasks{activity: map[string]time.Time{"bordu": now.Add(-2 * time.Hour)}}

	c := &Checker{Desktops: desktops, Tasks: tasks, Threshold: time.Hour, Action: "pause", now: func() time.Time { return now }}
	c.sweep(context.Background())

	if len(desktops.paused) != 1 || desktops.paused[0] != "bordu" {
		t.Fatalf("paused = %v, want [bordu]", desktops.paused)
	}
	if len(desktops.stopped) != 0 {
		t.Fatalf("stopped = %v, want []", desktops.stopped)
	}
}

func TestCheckFastPause(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	d := desktop.Desktop{ID: "bordu", Status: "running", StartedAt: now.Add(-day).Format(time.RFC3339Nano)}
	desktops := &fakeDesktops{list: []desktop.Desktop{d}}
	tasks := &fakeTasks{activity: map[string]time.Time{"bordu": now.Add(-30 * time.Second)}}

	c := &Checker{Desktops: desktops, Tasks: tasks, Threshold: time.Hour, Action: "pause", now: func() time.Time { return now }}
	c.checkFastPause(context.Background(), d, 15*time.Second)

	if len(desktops.paused) != 1 || desktops.paused[0] != "bordu" {
		t.Fatalf("fast-pause: paused = %v, want [bordu]", desktops.paused)
	}
}
