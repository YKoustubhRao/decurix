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

// TestAnIdleSweepLeavesTheControlPlanesOwnWorkers: a readiness run or a practice desktop (Simulate's, or an eval's) has
// no desktop container, and a sweep while it runs must not end it; at start-up nothing of theirs is still running, so
// they are closed then. The workers are as their runners register them (the eval's by its engine "eval").
func TestAnIdleSweepLeavesTheControlPlanesOwnWorkers(t *testing.T) {
	workers := map[string]tasks.Worker{
		"readiness-1": {ID: "readiness-1", Engine: "qualified-tool", Profile: "offline-artifacts"},
		"practice-2":  {ID: "practice-2", Engine: "practice", Profile: "practice"},
		"practice-e3": {ID: "practice-e3", Engine: "eval", Profile: "practice"},
		"box":         {ID: "box", Engine: "thebot", Profile: "basic"},
	}
	ids := []string{"readiness-1", "practice-2", "practice-e3", "box"}
	gone := fakeGone{gone: map[string]bool{"readiness-1": true, "practice-2": true, "practice-e3": true, "box": true}}
	sweep := &fakeLive{live: ids, workers: workers}
	closeGone(context.Background(), gone, sweep, false)
	if len(sweep.closed) != 1 || sweep.closed[0] != "box" {
		t.Fatalf("an idle sweep closed %v, want only the desktop", sweep.closed)
	}
	start := &fakeLive{live: ids, workers: workers}
	CloseGoneAtStart(context.Background(), gone, start)
	if len(start.closed) != 4 {
		t.Fatalf("at start-up closed %v, want all four", start.closed)
	}
}

// TestAStoreThatCannotListLiveWorkersClosesNothing: the reconcile needs LiveWorkers; without it nothing is guessed.
func TestAStoreThatCannotListLiveWorkersClosesNothing(t *testing.T) {
	CloseGoneAtStart(context.Background(), fakeGone{gone: map[string]bool{"x": true}}, &fakeTasks{})
}
