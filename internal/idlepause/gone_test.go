package idlepause

import (
	"context"
	"errors"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/tasks"
)

type fakeGone struct{ gone, errs map[string]bool }

func (f fakeGone) Gone(_ context.Context, id string) (bool, error) {
	if f.errs[id] {
		return false, errors.New("docker timed out")
	}
	return f.gone[id], nil
}

type fakeLive struct {
	live    []string
	workers map[string]tasks.Worker
	closed  []string
}

func (f *fakeLive) LiveWorkers() ([]string, error) { return f.live, nil }
func (f *fakeLive) Worker(id string) (tasks.Worker, bool) {
	w, ok := f.workers[id]
	return w, ok
}
func (f *fakeLive) CloseOpen(id, why string) error {
	if why == "" {
		return errors.New("no reason")
	}
	f.closed = append(f.closed, id)
	return nil
}

// TestTasksOfAGoneDesktopAreClosed: a worker with a running task and no container is closed at start-up (C-17); one
// whose container is there, or whose check failed, is left alone.
func TestTasksOfAGoneDesktopAreClosed(t *testing.T) {
	store := &fakeLive{live: []string{"gone-box", "here-box", "unsure-box"}}
	CloseGoneAtStart(context.Background(), fakeGone{
		gone: map[string]bool{"gone-box": true}, errs: map[string]bool{"unsure-box": true},
	}, store)
	if len(store.closed) != 1 || store.closed[0] != "gone-box" {
		t.Fatalf("closed %v, want only gone-box", store.closed)
	}
}

// TestAnIdleSweepLeavesTheControlPlanesOwnWorkers: a readiness run or a practice desktop has no desktop container,
// and a sweep while it runs must not end it; at start-up nothing of theirs is still running, so they are closed then.
func TestAnIdleSweepLeavesTheControlPlanesOwnWorkers(t *testing.T) {
	workers := map[string]tasks.Worker{
		"readiness-1": {ID: "readiness-1", Engine: "qualified-tool"},
		"practice-2":  {ID: "practice-2", Engine: "practice"},
		"box":         {ID: "box", Engine: "thebot"},
	}
	gone := fakeGone{gone: map[string]bool{"readiness-1": true, "practice-2": true, "box": true}}
	sweep := &fakeLive{live: []string{"readiness-1", "practice-2", "box"}, workers: workers}
	closeGone(context.Background(), gone, sweep, false)
	if len(sweep.closed) != 1 || sweep.closed[0] != "box" {
		t.Fatalf("an idle sweep closed %v, want only the desktop", sweep.closed)
	}
	start := &fakeLive{live: []string{"readiness-1", "practice-2", "box"}, workers: workers}
	CloseGoneAtStart(context.Background(), gone, start)
	if len(start.closed) != 3 {
		t.Fatalf("at start-up closed %v, want all three", start.closed)
	}
}

// TestAStoreThatCannotListLiveWorkersClosesNothing: the reconcile needs LiveWorkers; without it nothing is guessed.
func TestAStoreThatCannotListLiveWorkersClosesNothing(t *testing.T) {
	CloseGoneAtStart(context.Background(), fakeGone{gone: map[string]bool{"x": true}}, &fakeTasks{})
}
