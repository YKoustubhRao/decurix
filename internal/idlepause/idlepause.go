// Package idlepause periodically stops desktops that have sat idle - no task
// running, queued or waiting on a human, and no sign anyone has a screen or
// thumbnail open - for longer than a configured threshold. AO's cloud product
// does the same for its sandboxes, for the same reason: a full desktop VM
// costs real CPU and memory whether or not anyone is using it.
//
// On by default at a 5-minute threshold (see cmd/decurix/main.go's
// idleStopThreshold, DECURIX_IDLE_STOP_MINUTES); a non-positive Threshold -
// the explicit opt-out - makes Run a no-op. Even on, this package only ever
// takes the same Stop a human could click themselves - never delete, never
// anything a manual Stop does not already do (tasks.CloseOpen runs exactly
// as it would for a manual stop, since it is the same desktop.Manager.Stop
// call) - Decurix desktops are otherwise entirely user-managed
// (start/stop/restart/delete are the only other lifecycle transitions).
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
// have been idle past Threshold.
type Checker struct {
	Desktops  DesktopLister
	Tasks     ActivityStore
	Threshold time.Duration
	Interval  time.Duration // defaultInterval if zero
	Action    string        // "pause" (freeze at 0% CPU) or "stop" (default)

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

// sweep checks every running desktop, up to sweepConcurrency at once.
func (c *Checker) sweep(ctx context.Context) {
	desktops, err := c.Desktops.List(ctx)
	if err != nil {
		log.Printf("idlepause: listing desktops: %v", err)
		return
	}

	sem := make(chan struct{}, sweepConcurrency)
	var wg sync.WaitGroup

	for _, d := range desktops {
		if d.Status != "running" {
			continue
		}

		wg.Add(1)
		sem <- struct{}{}

		go func(d desktop.Desktop) {
			defer wg.Done()
			defer func() { <-sem }()

			c.checkOne(ctx, d)
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

	if c.Action == "pause" {
		log.Printf("idlepause: pausing %s - idle since %s", d.ID, idleSince.Format(time.RFC3339))
		if err := c.Desktops.Pause(ctx, d.ID); err != nil {
			log.Printf("idlepause: pausing %s: %v", d.ID, err)
		}
	} else {
		log.Printf("idlepause: stopping %s - idle since %s", d.ID, idleSince.Format(time.RFC3339))
		if err := c.Desktops.Stop(ctx, d.ID); err != nil {
			log.Printf("idlepause: stopping %s: %v", d.ID, err)
		}
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

	if c.Action == "pause" {
		log.Printf("idlepause: fast-pausing %s after task completion - idle since %s", d.ID, idleSince.Format(time.RFC3339))
		if err := c.Desktops.Pause(ctx, d.ID); err != nil {
			log.Printf("idlepause: fast-pausing %s: %v", d.ID, err)
		}
	} else {
		log.Printf("idlepause: fast-stopping %s after task completion - idle since %s", d.ID, idleSince.Format(time.RFC3339))
		if err := c.Desktops.Stop(ctx, d.ID); err != nil {
			log.Printf("idlepause: fast-stopping %s: %v", d.ID, err)
		}
	}
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
