package idlepause

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
)

// roomFake adds what tiered stops and making room need (RoomDesktops) to fakeDesktops: per-desktop locks, when each
// was paused, woken and last looked at before its pause, and why each rests.
type roomFake struct {
	*fakeDesktops

	locksMu sync.Mutex
	locks   map[string]*sync.Mutex

	pausedAt map[string]time.Time
	viewedAt map[string]time.Time
	wokenAt  map[string]time.Time

	restMu sync.Mutex
	rests  map[string]desktop.Rest
}

func newRoomFake(list ...desktop.Desktop) *roomFake {
	return &roomFake{fakeDesktops: &fakeDesktops{list: list}, locks: map[string]*sync.Mutex{},
		pausedAt: map[string]time.Time{}, viewedAt: map[string]time.Time{}, wokenAt: map[string]time.Time{},
		rests: map[string]desktop.Rest{}}
}

func (r *roomFake) lock(id string) *sync.Mutex {
	r.locksMu.Lock()
	defer r.locksMu.Unlock()
	mu, ok := r.locks[id]
	if !ok {
		mu = &sync.Mutex{}
		r.locks[id] = mu
	}
	return mu
}

func (r *roomFake) LockDesktop(id string) func() { mu := r.lock(id); mu.Lock(); return mu.Unlock }

func (r *roomFake) TryLockDesktop(id string) (func(), bool) {
	mu := r.lock(id)
	if !mu.TryLock() {
		return nil, false
	}
	return mu.Unlock, true
}

func (r *roomFake) PausedSince(id string) (time.Time, time.Time) {
	return r.pausedAt[id], r.viewedAt[id]
}

func (r *roomFake) LastWoken(id string) (time.Time, bool) { t, ok := r.wokenAt[id]; return t, ok }

func (r *roomFake) IsPaused(_ context.Context, id string) (bool, error) {
	for _, s := range r.stoppedIDs() {
		if s == id {
			return false, nil
		}
	}
	for _, d := range r.list {
		if d.ID == id {
			return d.Status == "paused", nil
		}
	}
	return false, errors.New("no such desktop")
}

func (r *roomFake) SetRest(id string, x desktop.Rest) {
	r.restMu.Lock()
	r.rests[id] = x
	r.restMu.Unlock()
}
func (r *roomFake) ClearRest(id string) { r.restMu.Lock(); delete(r.rests, id); r.restMu.Unlock() }

func (r *roomFake) stoppedIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.stopped...)
}

// fakeMemory is the Docker host's available memory: base, plus what each stopped desktop gave back.
type fakeMemory struct {
	base    int64
	freed   map[string]int64
	desks   *roomFake
	unknown bool
}

func (f *fakeMemory) AvailableMB(context.Context) (int64, bool) {
	if f.unknown {
		return 0, false
	}
	avail := f.base
	if f.desks != nil {
		for _, id := range f.desks.stoppedIDs() {
			avail += f.freed[id]
		}
	}
	return avail, true
}

var roomNow = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

// pausedDesk is a paused desktop, as a listing shows one (no StartedAt).
func pausedDesk(id string) desktop.Desktop {
	return desktop.Desktop{ID: id, Status: "paused", Profile: "basic"}
}

// roomChecker is a Checker over desks whose memory is mem, at roomNow.
func roomChecker(desks *roomFake, tasks ActivityStore, mem MemoryReader) *Checker {
	return &Checker{Desktops: desks, Tasks: tasks, Threshold: 5 * time.Minute, Action: "pause",
		StopAfter: DefaultStopAfter, Memory: mem, now: func() time.Time { return roomNow }}
}

// TestAPausedDesktopPastStopAfterIsStopped: tiered idle - paused for longer than StopAfter, it is stopped and says
// why; paused for less, it stays paused.
func TestAPausedDesktopPastStopAfterIsStopped(t *testing.T) {
	desks := newRoomFake(pausedDesk("old"), pausedDesk("new"))
	desks.pausedAt["old"] = roomNow.Add(-31 * time.Minute)
	desks.pausedAt["new"] = roomNow.Add(-10 * time.Minute)
	c := roomChecker(desks, &fakeTasks{}, nil)
	c.sweep(context.Background())
	if got := desks.stoppedIDs(); len(got) != 1 || got[0] != "old" {
		t.Fatalf("stopped %v, want [old]", got)
	}
	if r := desks.rests["old"]; r.Reason != desktop.RestIdle || r.Note != "Stopped after 30 minutes paused, to save memory" {
		t.Fatalf("rest %+v", r)
	}
	if len(desks.paused) != 0 {
		t.Fatalf("paused %v", desks.paused)
	}
}

// TestZeroStopAfterKeepsPausedDesktopsPaused: DECURIX_IDLE_STOP_AFTER_MINUTES=0 never stops one.
func TestZeroStopAfterKeepsPausedDesktopsPaused(t *testing.T) {
	desks := newRoomFake(pausedDesk("old"))
	desks.pausedAt["old"] = roomNow.Add(-24 * time.Hour)
	c := roomChecker(desks, &fakeTasks{}, nil)
	c.StopAfter = 0
	c.sweep(context.Background())
	if got := desks.stoppedIDs(); len(got) != 0 {
		t.Fatalf("stopped %v", got)
	}
}

// neverRestCases are desktops that are never stopped to give memory back, each with its tasks store.
func neverRestCases() map[string]func() (desktop.Desktop, *roomFake, *fakeTasks) {
	long := roomNow.Add(-3 * time.Hour)
	base := func(d desktop.Desktop) *roomFake {
		r := newRoomFake(d)
		r.pausedAt[d.ID] = long
		return r
	}
	return map[string]func() (desktop.Desktop, *roomFake, *fakeTasks){
		"busy with a task": func() (desktop.Desktop, *roomFake, *fakeTasks) {
			d := pausedDesk("w")
			return d, base(d), &fakeTasks{busy: map[string]string{"w": "a task is still running on this desktop"}}
		},
		"holding an open question or approval": func() (desktop.Desktop, *roomFake, *fakeTasks) {
			d := pausedDesk("w")
			return d, base(d), &fakeTasks{busy: map[string]string{"w": "the agent is waiting for your reply to a task"}}
		},
		"recording a ticket": func() (desktop.Desktop, *roomFake, *fakeTasks) {
			// A ticket is an open execution of a demonstration task until End.
			d := pausedDesk("w")
			return d, base(d), &fakeTasks{busy: map[string]string{"w": "a task is still running on this desktop"}}
		},
		"viewed just before its pause": func() (desktop.Desktop, *roomFake, *fakeTasks) {
			d := pausedDesk("w")
			r := base(d)
			r.viewedAt["w"] = roomNow.Add(-2 * time.Minute)
			return d, r, &fakeTasks{}
		},
		"a practice desktop": func() (desktop.Desktop, *roomFake, *fakeTasks) {
			d := pausedDesk("p")
			d.Profile = "practice"
			return d, base(d), &fakeTasks{}
		},
		"a readiness desktop by its name": func() (desktop.Desktop, *roomFake, *fakeTasks) {
			d := pausedDesk("readiness-r1")
			return d, base(d), &fakeTasks{}
		},
		"started or woken by a person minutes ago": func() (desktop.Desktop, *roomFake, *fakeTasks) {
			d := pausedDesk("w")
			r := base(d)
			r.wokenAt["w"] = roomNow.Add(-3 * time.Minute)
			return d, r, &fakeTasks{}
		},
		"a task that just ended": func() (desktop.Desktop, *roomFake, *fakeTasks) {
			d := pausedDesk("w")
			return d, base(d), &fakeTasks{activity: map[string]time.Time{"w": roomNow.Add(-time.Minute)}}
		},
		"running, not paused": func() (desktop.Desktop, *roomFake, *fakeTasks) {
			d := desktop.Desktop{ID: "w", Status: "running", StartedAt: roomNow.Add(-time.Minute).Format(time.RFC3339Nano)}
			return d, base(d), &fakeTasks{}
		},
	}
}

// TestNeverRestCasesAreNeverStopped: neither the tiered stop nor making room stops any of them, and making room with
// none to stop refuses the start.
func TestNeverRestCasesAreNeverStopped(t *testing.T) {
	for name, mk := range neverRestCases() {
		t.Run(name, func(t *testing.T) {
			_, desks, tasks := mk()
			mem := &fakeMemory{base: 200, freed: map[string]int64{"w": 700, "p": 700, "readiness-r1": 700}, desks: desks}
			c := roomChecker(desks, tasks, mem)
			c.sweep(context.Background())
			err := c.MakeRoom(context.Background(), "nova", 700)
			if got := desks.stoppedIDs(); len(got) != 0 {
				t.Fatalf("stopped %v", got)
			}
			if !errors.Is(err, desktop.ErrNoRoom) {
				t.Fatalf("MakeRoom gave %v, want the no-room refusal", err)
			}
		})
	}
}

// TestMakeRoomStopsTheLongestIdleOnlyAsNeeded: with three paused desktops, the longest idle go first and only as
// many as it takes to cover the start plus the floor; each says it made room.
func TestMakeRoomStopsTheLongestIdleOnlyAsNeeded(t *testing.T) {
	desks := newRoomFake(pausedDesk("b"), pausedDesk("a"), pausedDesk("c"))
	desks.pausedAt["a"] = roomNow.Add(-3 * time.Hour)
	desks.pausedAt["b"] = roomNow.Add(-1 * time.Hour)
	desks.pausedAt["c"] = roomNow.Add(-2 * time.Hour)
	mem := &fakeMemory{base: 300, freed: map[string]int64{"a": 600, "b": 600, "c": 600}, desks: desks}
	c := roomChecker(desks, &fakeTasks{}, mem)
	// 300 free; 700 + 512 wanted: a (300+600=900) is not enough, c too (1500) is.
	if err := c.MakeRoom(context.Background(), "nova", 700); err != nil {
		t.Fatal(err)
	}
	got := desks.stoppedIDs()
	if len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Fatalf("stopped %v, want [a c]", got)
	}
	for _, id := range got {
		if r := desks.rests[id]; r.Reason != desktop.RestRoom || r.Note != "Stopped to make room for nova, to save memory" {
			t.Fatalf("%s rests %+v", id, r)
		}
	}
}

// TestMakeRoomWithEnoughMemoryStopsNothing: a start that fits stops nobody.
func TestMakeRoomWithEnoughMemoryStopsNothing(t *testing.T) {
	desks := newRoomFake(pausedDesk("a"))
	desks.pausedAt["a"] = roomNow.Add(-3 * time.Hour)
	c := roomChecker(desks, &fakeTasks{}, &fakeMemory{base: 4000})
	if err := c.MakeRoom(context.Background(), "nova", 700); err != nil {
		t.Fatal(err)
	}
	if got := desks.stoppedIDs(); len(got) != 0 {
		t.Fatalf("stopped %v", got)
	}
}

// TestMakeRoomWithUnknownMemoryStartsAsBefore: memory that cannot be read stops nobody and refuses nothing.
func TestMakeRoomWithUnknownMemoryStartsAsBefore(t *testing.T) {
	desks := newRoomFake(pausedDesk("a"))
	desks.pausedAt["a"] = roomNow.Add(-3 * time.Hour)
	c := roomChecker(desks, &fakeTasks{}, &fakeMemory{unknown: true})
	if err := c.MakeRoom(context.Background(), "nova", 700); err != nil {
		t.Fatal(err)
	}
	if got := desks.stoppedIDs(); len(got) != 0 {
		t.Fatalf("stopped %v", got)
	}
}

// TestNothingStoppableRefusesClearly: with only busy desktops and no room for the desktop itself, the start is refused
// with how many are busy; with room for the desktop but not the floor, it goes ahead.
func TestNothingStoppableRefusesClearly(t *testing.T) {
	busy := &fakeTasks{busy: map[string]string{"x": "a task is still running", "y": "a task is still running"}}
	desks := newRoomFake(pausedDesk("x"), desktop.Desktop{ID: "y", Status: "running"}, desktop.Desktop{ID: "z", Status: "stopped"})
	c := roomChecker(desks, busy, &fakeMemory{base: 400})
	err := c.MakeRoom(context.Background(), "nova", 700)
	var no *desktop.NoRoomError
	if !errors.As(err, &no) || !strings.HasPrefix(err.Error(), "Not enough memory to start this desktop: 2 desktops are busy.") {
		t.Fatalf("got %v", err)
	}

	c = roomChecker(desks, busy, &fakeMemory{base: 900})
	if err := c.MakeRoom(context.Background(), "nova", 700); err != nil {
		t.Fatalf("a desktop that fits under the floor was refused: %v", err)
	}
}

// TestTwoStartsDoNotCountTheSameMemoryTwice: a start let through counts against the free memory while its desktop
// boots, so a second start right after it is not let through on the same megabytes.
func TestTwoStartsDoNotCountTheSameMemoryTwice(t *testing.T) {
	c := roomChecker(newRoomFake(), &fakeTasks{}, &fakeMemory{base: 1300})
	if err := c.MakeRoom(context.Background(), "one", 700); err != nil {
		t.Fatal(err)
	}
	if err := c.MakeRoom(context.Background(), "two", 700); !errors.Is(err, desktop.ErrNoRoom) {
		t.Fatalf("the second start was let through on the first one's memory: %v", err)
	}
	c.now = func() time.Time { return roomNow.Add(2 * reserveFor) }
	if err := c.MakeRoom(context.Background(), "two", 700); err != nil {
		t.Fatalf("once the first had booted, the second was refused: %v", err)
	}
}

// TestARestartDoesNotCountItsOwnEarlierStart: a desktop started again (a practice desktop settled between the cases
// of a learning curve, every few seconds) is not refused on reservations of its own earlier boots, which the restart
// ended; another desktop's start still counts.
func TestARestartDoesNotCountItsOwnEarlierStart(t *testing.T) {
	c := roomChecker(newRoomFake(), &fakeTasks{}, &fakeMemory{base: 1300})
	for i := 0; i < 5; i++ {
		if err := c.MakeRoom(context.Background(), "practice", 700); err != nil {
			t.Fatalf("restart %d was refused on its own earlier starts: %v", i+1, err)
		}
	}
	if err := c.MakeRoom(context.Background(), "two", 700); !errors.Is(err, desktop.ErrNoRoom) {
		t.Fatalf("another desktop's start was let through on the restarted one's memory: %v", err)
	}
}

// TestARequestHoldingTheLockWinsOverMakingRoom: a desktop whose lifecycle lock is held (a request being delivered to
// it) is passed over, not waited for, and the next one is stopped instead.
func TestARequestHoldingTheLockWinsOverMakingRoom(t *testing.T) {
	desks := newRoomFake(pausedDesk("a"), pausedDesk("b"))
	desks.pausedAt["a"] = roomNow.Add(-3 * time.Hour)
	desks.pausedAt["b"] = roomNow.Add(-1 * time.Hour)
	mem := &fakeMemory{base: 300, freed: map[string]int64{"a": 1000, "b": 1000}, desks: desks}
	c := roomChecker(desks, &fakeTasks{}, mem)
	unlock := desks.LockDesktop("a") // a delivery to a in flight
	defer unlock()
	done := make(chan error, 1)
	go func() { done <- c.MakeRoom(context.Background(), "nova", 700) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("making room waited for a lock a request holds")
	}
	if got := desks.stoppedIDs(); len(got) != 1 || got[0] != "b" {
		t.Fatalf("stopped %v, want [b]", got)
	}
}

// TestATaskDeliveredDuringTheTieredStopWins: the sweep saw the desktop paused and idle, then a task was delivered
// under its lock; the stop, under the same lock, sees the task and leaves the desktop alone (B-23).
func TestATaskDeliveredDuringTheTieredStopWins(t *testing.T) {
	desks := newRoomFake(pausedDesk("box"))
	desks.pausedAt["box"] = roomNow.Add(-time.Hour)
	tasks := &flipTasks{}
	c := roomChecker(desks, tasks, nil)
	unlock := desks.LockDesktop("box")
	done := make(chan struct{})
	go func() { c.checkPaused(context.Background(), pausedDesk("box")); close(done) }()
	time.Sleep(20 * time.Millisecond)
	tasks.mu.Lock()
	tasks.busy = true
	tasks.mu.Unlock()
	unlock()
	<-done
	if got := desks.stoppedIDs(); len(got) != 0 {
		t.Fatalf("a desktop given a task was stopped: %v", got)
	}
}

// TestMakeRoomNeverStopsTheDesktopItStarts: the desktop being started is never a candidate, even if listed paused.
func TestMakeRoomNeverStopsTheDesktopItStarts(t *testing.T) {
	desks := newRoomFake(pausedDesk("nova"), pausedDesk("a"))
	desks.pausedAt["nova"] = roomNow.Add(-5 * time.Hour)
	desks.pausedAt["a"] = roomNow.Add(-1 * time.Hour)
	mem := &fakeMemory{base: 300, freed: map[string]int64{"nova": 1000, "a": 1000}, desks: desks}
	c := roomChecker(desks, &fakeTasks{}, mem)
	if err := c.MakeRoom(context.Background(), "nova", 700); err != nil {
		t.Fatal(err)
	}
	got := desks.stoppedIDs()
	sort.Strings(got)
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("stopped %v, want [a]", got)
	}
}
