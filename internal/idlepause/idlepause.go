// Package idlepause periodically pauses desktops that have sat idle - no task
// running, queued or waiting on a human, and no sign anyone has a screen or
// thumbnail open - for longer than a configured threshold, and once more a
// short while after a task finishes (FastPauseAfter). AO's cloud product does
// the same for its sandboxes, for the same reason: a full desktop VM costs
// real CPU and memory whether or not anyone is using it.
//
// On by default at a 5-minute threshold (see cmd/decurix/main.go's
// idleStopThreshold, DECURIX_IDLE_STOP_MINUTES); a non-positive Threshold -
// the explicit opt-out - makes Run a no-op. The default action is a pause
// (desktop.Manager.Pause: the cgroup freezer, 0% CPU, memory kept, resumed
// on the next request); DECURIX_IDLE_ACTION=stop stops instead. Either way
// this package only ever takes the same Pause or Stop a human could click
// themselves - never delete, never anything a manual one does not already do
// (a stop runs tasks.CloseOpen exactly as a manual stop does, since it is
// the same desktop.Manager.Stop call) - Decurix desktops are otherwise
// entirely user-managed.
//
// A desktop left paused for StopAfter (30 minutes by default) is then stopped, and before a desktop starts MakeRoom
// stops the longest-idle paused desktops when the Docker host's memory would not hold it (room.go, lightweight L2).
package idlepause

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
)

// DesktopLister is what this package needs from desktop.Manager: list
// desktops, stop one, and say when one was last actually looked at (its
// thumbnail fetched, or its screen proxied to). Satisfied by *desktop.Manager;
// narrowed to an interface so a fake can stand in for tests that have no
// Docker daemon to talk to.
type DesktopLister interface {
	List(ctx context.Context) ([]desktop.Desktop, error)
	Stop(ctx context.Context, id string) error
	Pause(ctx context.Context, id string) error
	LastViewed(id string) (time.Time, bool)
}

// ActivityStore is what this package needs from tasks.Store: whether a
// worker is currently busy, and when its task records last changed.
// Satisfied by *tasks.Store.
type ActivityStore interface {
	Busy(workerID string) (string, error)
	LastActivity(workerID string) (time.Time, bool, error)
}

// Checker sweeps running desktops on an interval and stops or pauses the ones that
// have been idle past Threshold, and stops paused ones past StopAfter.
type Checker struct {
	Desktops  DesktopLister
	Tasks     ActivityStore
	Threshold time.Duration
	Interval  time.Duration // defaultInterval if zero
	Action    string        // "stop", or anything else: "pause" (freeze at 0% CPU, the default)

	// StopAfter is how long a desktop stays paused before the sweep stops it to give its memory back (room.go,
	// DECURIX_IDLE_STOP_AFTER_MINUTES); zero or less never does.
	StopAfter time.Duration
	// Memory reads the Docker host's available memory for MakeRoom; nil makes no room (desktops start as before).
	Memory MemoryReader
	// FloorMB is the memory MakeRoom keeps free after a start; DefaultFloorMB if zero.
	FloorMB int64

	// roomMu makes room for one start at a time; reserved is what starts let through lately are taken to use.
	roomMu   sync.Mutex
	reserved []reservation

	// now is the test seam - defaults to time.Now.
	now func() time.Time
}

// defaultInterval is how often Run sweeps when Interval is unset.
const defaultInterval = 2 * time.Minute

// sweepConcurrency bounds how many desktops sweep checks (and, when idle,
// stops) at once. A burst of desktops crossing the idle threshold in the
// same tick must not be stopped one at a time - each Stop can itself take
// up to DESKTOP_STOP_TIMEOUT_SECONDS (10s default) waiting for a graceful
// shutdown, so a sequential sweep of many desktops could take minutes.
const sweepConcurrency = 8

// Run sweeps on Interval until ctx is done. A Threshold of zero or less
// disables the checker entirely - Run returns immediately rather than
// looping forever doing nothing, so a caller can always start it
// unconditionally and let the threshold decide.
func (c *Checker) Run(ctx context.Context) {
	if c.Threshold <= 0 {
		return
	}

	interval := c.Interval
	if interval <= 0 {
		interval = defaultInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		c.sweep(ctx)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// sweep checks every running desktop, up to sweepConcurrency at once, after closing the tasks of desktops that are
// gone (gone.go).
func (c *Checker) sweep(ctx context.Context) {
	if g, ok := c.Desktops.(DesktopGone); ok {
		closeGone(ctx, g, c.Tasks, false)
	}
	desktops, err := c.Desktops.List(ctx)
	if err != nil {
		log.Printf("idlepause: listing desktops: %v", err)
		return
	}

	sem := make(chan struct{}, sweepConcurrency)
	var wg sync.WaitGroup

	for _, d := range desktops {
		check := c.checkOne
		switch {
		case d.Status == "running":
		case d.Status == "paused" && c.StopAfter > 0:
			check = c.checkPaused // paused long enough: stopped to give its memory back (room.go)
		default:
			continue
		}

		wg.Add(1)
		sem <- struct{}{}

		go func(d desktop.Desktop) {
			defer wg.Done()
			defer func() { <-sem }()

			check(ctx, d)
		}(d)
	}

	wg.Wait()
}

// checkOne stops d if it is idle past Threshold, and does nothing otherwise -
// including when there simply is not enough signal to tell (no StartedAt),
// which is treated as "leave it alone", never as "assume idle".
func (c *Checker) checkOne(ctx context.Context, d desktop.Desktop) {
	if reason, err := c.Tasks.Busy(d.ID); err != nil {
		log.Printf("idlepause: checking %s: %v", d.ID, err)
		return
	} else if reason != "" {
		return
	}

	idleSince, ok, err := c.idleSince(d)
	if err != nil {
		log.Printf("idlepause: checking %s: %v", d.ID, err)
		return
	}
	if !ok {
		return
	}

	if c.clock().Sub(idleSince) < c.Threshold {
		return
	}

	c.idleOut(ctx, d.ID, idleSince, "", "")
}

// DesktopLocker is the per-desktop lifecycle lock (desktop.Manager.LockDesktop): held from the last look at whether a
// desktop is busy to the pause or stop, so a task delivered meanwhile is never frozen (B-23).
type DesktopLocker interface {
	LockDesktop(id string) (unlock func())
}

// idleOut pauses or stops (per Action) an idle desktop, logging it with prefix before the verb and why after the id.
// Under the desktop's lifecycle lock it asks once more whether the desktop is busy, and leaves it if it now is.
func (c *Checker) idleOut(ctx context.Context, id string, idleSince time.Time, prefix, why string) {
	if l, ok := c.Desktops.(DesktopLocker); ok {
		defer l.LockDesktop(id)()
	}
	if reason, err := c.Tasks.Busy(id); err != nil || reason != "" {
		return
	}
	// Only an explicit "stop" stops; anything else, a typo included, pauses (B-37): a stop ends the agent's work in
	// memory, a pause keeps it.
	verb, act := "pausing", c.Desktops.Pause
	if c.Action == "stop" {
		verb, act = "stopping", c.Desktops.Stop
	}
	log.Printf("idlepause: %s%s %s%s - idle since %s", prefix, verb, id, why, idleSince.Format(time.RFC3339))
	if err := act(ctx, id); err != nil {
		log.Printf("idlepause: %s%s %s: %v", prefix, verb, id, err)
	}
}

// FastPauseAfter schedules an idle check for desktopID after delay (e.g. 15 seconds after a task finishes).
// If the desktop is not busy and hasn't had new activity or views during that window, it transitions to idle immediately.
func (c *Checker) FastPauseAfter(desktopID string, delay time.Duration) {
	if c == nil || c.Threshold <= 0 {
		return
	}
	go func() {
		time.Sleep(delay)
		ctx := context.Background()
		desktops, err := c.Desktops.List(ctx)
		if err != nil {
			return
		}
		for _, d := range desktops {
			if d.ID == desktopID && d.Status == "running" {
				c.checkFastPause(ctx, d, delay)
				return
			}
		}
	}()
}

func (c *Checker) checkFastPause(ctx context.Context, d desktop.Desktop, minIdle time.Duration) {
	if reason, err := c.Tasks.Busy(d.ID); err != nil || reason != "" {
		return
	}

	idleSince, ok, err := c.idleSince(d)
	if err != nil || !ok {
		return
	}

	if c.clock().Sub(idleSince) < minIdle {
		return
	}

	c.idleOut(ctx, d.ID, idleSince, "fast-", " after task completion")
}

// idleSince is the latest of: when the desktop last started, when its task
// records last changed, and when its thumbnail or screen was last looked at -
// the point after which "idle" can actually start being measured from. ok is
// false only when the desktop's start time itself is unknown, since that is
// the one signal every running desktop must have. A LastActivity store error
// fails safe the same way Busy's does: reported as an error rather than
// silently falling back to the other two signals, since proceeding on
// incomplete data here is what could stop a desktop mid-use rather than just
// leave one alone.
func (c *Checker) idleSince(d desktop.Desktop) (time.Time, bool, error) {
	started, ok := parseStartedAt(d.StartedAt)
	if !ok {
		return time.Time{}, false, nil
	}

	since := started

	if last, found, err := c.Tasks.LastActivity(d.ID); err != nil {
		return time.Time{}, false, err
	} else if found && last.After(since) {
		since = last
	}

	if last, found := c.Desktops.LastViewed(d.ID); found && last.After(since) {
		since = last
	}

	return since, true, nil
}

// parseStartedAt reads Docker's RFC3339Nano State.StartedAt, treating both a
// parse failure and Docker's zero-value "never started" timestamp as no
// signal.
func parseStartedAt(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}

	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || t.Year() <= 1 {
		return time.Time{}, false
	}

	return t, true
}

func (c *Checker) clock() time.Time {
	if c.now != nil {
		return c.now()
	}

	return time.Now()
}
