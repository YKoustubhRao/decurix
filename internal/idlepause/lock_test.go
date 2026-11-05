package idlepause

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
)

type lockingDesktops struct {
	*fakeDesktops
	mu sync.Mutex
}

func (l *lockingDesktops) LockDesktop(string) func() { l.mu.Lock(); return l.mu.Unlock }

type flipTasks struct {
	fakeTasks
	mu   sync.Mutex
	busy bool
}

func (f *flipTasks) Busy(string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.busy {
		return "a task is running", nil
	}
	return "", nil
}

// TestATaskDeliveredDuringTheSweepIsNotFrozen: the sweep saw the desktop idle, then a task was delivered (under the
// desktop's lock); the pause, taken under the same lock, sees the task and leaves the desktop alone (B-23).
func TestATaskDeliveredDuringTheSweepIsNotFrozen(t *testing.T) {
	d := &lockingDesktops{fakeDesktops: &fakeDesktops{list: []desktop.Desktop{{ID: "box", Status: "running"}}}}
	tasks := &flipTasks{}
	c := &Checker{Desktops: d, Tasks: tasks, Threshold: time.Minute, Action: "pause"}
	unlock := d.LockDesktop("box") // a delivery in flight
	done := make(chan struct{})
	go func() { c.idleOut(context.Background(), "box", time.Now(), "", ""); close(done) }()
	time.Sleep(20 * time.Millisecond)
	tasks.mu.Lock()
	tasks.busy = true // the delivery recorded its task
	tasks.mu.Unlock()
	unlock()
	<-done
	if len(d.paused) != 0 || len(d.stopped) != 0 {
		t.Fatalf("a desktop given a task was paused %v / stopped %v", d.paused, d.stopped)
	}
}

// TestAnUnknownIdleActionPauses: only "stop" stops (B-37).
func TestAnUnknownIdleActionPauses(t *testing.T) {
	d := &fakeDesktops{}
	c := &Checker{Desktops: d, Tasks: &fakeTasks{}, Action: "sotp"}
	c.idleOut(context.Background(), "box", time.Now(), "", "")
	if len(d.paused) != 1 || len(d.stopped) != 0 {
		t.Fatalf("paused %v stopped %v", d.paused, d.stopped)
	}
}
