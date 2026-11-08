package idlepause

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
)

// Memory back from idle desktops (lightweight L2). A paused desktop frees its CPU but keeps all its memory (about
// 400 MB, 630 MB with a browser), so on a 5 GB WSL VM only a handful fit. Two things give it back, both by the same
// Stop a person's button does (the home volume and everything on disk stay; the desktop starts again by itself when
// it is given work):
//
//   - Tiered idle: a desktop the sweep finds paused for StopAfter (DECURIX_IDLE_STOP_AFTER_MINUTES, 30 by default) is
//     stopped. The pause step before it is unchanged.
//   - Making room: before a desktop starts, MakeRoom (desktop.Manager's RoomMaker) stops the longest-idle paused
//     desktops, only as many as it takes, until the Docker host's available memory covers the start plus a floor. With
//     nothing left to stop it lets the start go ahead if the desktop itself fits, and otherwise refuses it with a
//     desktop.NoRoomError ("not enough memory: N desktops are busy").
//
// Never stopped (restable): a desktop that is not paused (a running one is in use or about to be paused); one busy
// with a task, a ticket being recorded (an open execution) or an open question or approval (tasks.Store.Busy); a
// practice, eval or readiness desktop; and one that was started or woken, looked at before its pause, or had task
// activity within protectGrace. A paused desktop's own views are not counted: opening its screen wakes it, so what
// looks at a paused desktop is its frozen thumbnail on the dashboard, and counting that would keep every paused
// desktop from resting while the dashboard is open. Each stop is taken under the desktop's lifecycle lock with Busy
// asked again, as the pause is (B-23); making room only tries the lock, so a request holding it wins and that desktop
// is passed over.

// RoomDesktops is what tiered stops and making room need from desktop.Manager beyond DesktopLister. Satisfied by
// *desktop.Manager.
type RoomDesktops interface {
	LockDesktop(id string) (unlock func())
	TryLockDesktop(id string) (unlock func(), ok bool)
	PausedSince(id string) (at, viewedBefore time.Time)
	LastWoken(id string) (time.Time, bool)
	IsPaused(ctx context.Context, id string) (bool, error)
	SetRest(id string, r desktop.Rest)
	ClearRest(id string)
}

// MemoryReader is the Docker host's available memory in MB, and whether it is known (resourcemon.HostMemory).
type MemoryReader interface {
	AvailableMB(ctx context.Context) (int64, bool)
}

const (
	// DefaultStopAfter is how long a desktop stays paused before it is stopped, when DECURIX_IDLE_STOP_AFTER_MINUTES
	// is unset: long enough that a person coming back within a meeting finds their windows as they left them.
	DefaultStopAfter = 30 * time.Minute

	// DefaultFloorMB is the memory left free after a start: room for the control plane, Docker and the kernel's page
	// cache, and for running desktops to grow (a browser opening) without the OOM killer choosing one.
	DefaultFloorMB = 512

	// protectGrace is how recently a desktop must have been started, woken, looked at or worked on to be left alone.
	protectGrace = 10 * time.Minute

	// reserveFor is how long a start that was let through counts against the available memory: a desktop takes
	// about this long to boot to its footprint, and two starts at once must not both count the same free memory.
	reserveFor = 90 * time.Second

	// restStopTimeout bounds one stop made to give memory back, apart from the start request's own context.
	restStopTimeout = 60 * time.Second
)

// reservation is memory a start that was let through is taken to use until its desktop has booted.
type reservation struct {
	id    string
	mb    int64
	until time.Time
}

// checkPaused stops paused desktop d once it has been paused for StopAfter and may rest (restable).
func (c *Checker) checkPaused(ctx context.Context, d desktop.Desktop) {
	rd, ok := c.Desktops.(RoomDesktops)
	if !ok || c.StopAfter <= 0 {
		return
	}
	pausedAt, _ := rd.PausedSince(d.ID)
	if c.clock().Sub(pausedAt) < c.StopAfter {
		return
	}
	if !c.restable(ctx, rd, d) {
		return
	}
	defer rd.LockDesktop(d.ID)()
	note := fmt.Sprintf("Stopped after %s paused, to save memory", roundMinutes(c.StopAfter))
	c.rest(ctx, rd, d, desktop.Rest{Reason: desktop.RestIdle, Note: note},
		fmt.Sprintf("paused since %s, past %s", pausedAt.Format(time.RFC3339), c.StopAfter))
}

// MakeRoom is desktop.Manager's RoomMaker: it frees memory for desktop forID, about to start and needing needMB,
// stopping the longest-idle paused desktops that may rest until the Docker host's available memory covers needMB plus
// the floor. Unknown memory lets the start go ahead, as before. With nothing left to stop, a start that still fits
// without the floor goes ahead, and one that does not is refused with a desktop.NoRoomError.
func (c *Checker) MakeRoom(ctx context.Context, forID string, needMB int64) error {
	if c == nil || c.Memory == nil {
		return nil
	}
	c.roomMu.Lock()
	defer c.roomMu.Unlock()

	avail, known := c.Memory.AvailableMB(ctx)
	if !known {
		return nil
	}
	now := c.clock()
	floor := c.FloorMB
	if floor <= 0 {
		floor = DefaultFloorMB
	}
	want := needMB + floor
	// A desktop started again (a restart, a practice desktop settled between cases) does not count its own earlier
	// start: that boot was ended, and what it used is in the reading or gone.
	self := desktop.SanitizeID(forID)
	avail -= c.reservedMB(now, self)
	if avail >= want {
		c.reserve(self, needMB, now)
		return nil
	}

	list, err := c.Desktops.List(ctx)
	if err != nil {
		log.Printf("idlepause: listing desktops to make room for %s: %v", forID, err)
	}
	stopped := map[string]bool{}
	if rd, ok := c.Desktops.(RoomDesktops); ok {
		for _, d := range c.restCandidates(ctx, rd, list, self) {
			if avail >= want {
				break
			}
			unlock, ok := rd.TryLockDesktop(d.ID)
			if !ok {
				continue // a request or a person is acting on it: it wins
			}
			done := c.rest(ctx, rd, d, desktop.Rest{Reason: desktop.RestRoom, Note: "Stopped to make room for " + forID + ", to save memory"},
				fmt.Sprintf("to make room for %s (%d MB free, %d MB wanted)", forID, avail, want))
			unlock()
			if !done {
				continue
			}
			stopped[d.ID] = true
			if a, ok := c.Memory.AvailableMB(ctx); ok {
				avail = a - c.reservedMB(now, self)
			} else {
				avail = want // the reading went away: start as before
			}
		}
	}
	if avail >= want {
		c.reserve(self, needMB, now)
		return nil
	}
	if avail >= needMB {
		log.Printf("idlepause: starting %s with %d MB free, under the %d MB floor: nothing more could be stopped", forID, avail, floor)
		c.reserve(self, needMB, now)
		return nil
	}
	busy := 0
	for _, d := range list {
		if (d.Status == "running" || d.Status == "paused") && desktop.SanitizeID(d.ID) != self && !stopped[d.ID] {
			busy++
		}
	}
	log.Printf("idlepause: not starting %s: %d MB free, it needs %d MB, and none of the %d running or paused desktops may be stopped", forID, avail, needMB, busy)
	return &desktop.NoRoomError{Busy: busy, FreeMB: avail, NeedMB: needMB}
}

// restCandidates are the desktops in list that may rest, other than self, longest idle first.
func (c *Checker) restCandidates(ctx context.Context, rd RoomDesktops, list []desktop.Desktop, self string) []desktop.Desktop {
	type cand struct {
		d     desktop.Desktop
		since time.Time
	}
	var out []cand
	for _, d := range list {
		if desktop.SanitizeID(d.ID) == self || !c.restable(ctx, rd, d) {
			continue
		}
		out = append(out, cand{d, c.lastUse(rd, d)})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].since.Before(out[j].since) })
	ds := make([]desktop.Desktop, len(out))
	for i, x := range out {
		ds[i] = x.d
	}
	return ds
}

// lastUse is the latest sign that paused desktop d was used: its start, a wake, its last task activity, its last look
// before the pause. A desktop with none is taken to have been idle since its pause.
func (c *Checker) lastUse(rd RoomDesktops, d desktop.Desktop) time.Time {
	pausedAt, viewed := rd.PausedSince(d.ID)
	var since time.Time
	later := func(t time.Time) {
		if t.After(since) {
			since = t
		}
	}
	if t, ok := parseStartedAt(d.StartedAt); ok {
		later(t)
	}
	if t, ok := rd.LastWoken(d.ID); ok {
		later(t)
	}
	if t, ok, err := c.Tasks.LastActivity(d.ID); err == nil && ok {
		later(t)
	}
	later(viewed)
	if since.IsZero() {
		since = pausedAt
	}
	return since
}

// restable says whether desktop d may be stopped to give its memory back (see the comment at the top of this file).
// A store error is "no". A paused desktop has no StartedAt in a listing, so a start is known from LastWoken, which a
// restarted control plane does not have: a desktop it finds paused may rest at once, as it has been idle at least
// the pause threshold or was paused by a person.
func (c *Checker) restable(_ context.Context, rd RoomDesktops, d desktop.Desktop) bool {
	if d.Status != "paused" || ownWorkerProfiles[d.Profile] || desktop.ReservedName(d.ID) {
		return false
	}
	if reason, err := c.Tasks.Busy(d.ID); err != nil || reason != "" {
		return false
	}
	now := c.clock()
	recent := func(t time.Time) bool { return !t.IsZero() && now.Sub(t) < protectGrace }
	if t, ok := parseStartedAt(d.StartedAt); ok && recent(t) {
		return false
	}
	if t, ok := rd.LastWoken(d.ID); ok && recent(t) {
		return false
	}
	if _, viewed := rd.PausedSince(d.ID); recent(viewed) {
		return false
	}
	t, ok, err := c.Tasks.LastActivity(d.ID)
	if err != nil || (ok && recent(t)) {
		return false
	}
	return true
}

// rest stops desktop d to give its memory back, the caller holding its lifecycle lock: it is asked once more whether
// it is still paused and may rest, its card is told why (r), one line is logged, and true is returned once it stopped.
func (c *Checker) rest(ctx context.Context, rd RoomDesktops, d desktop.Desktop, r desktop.Rest, why string) bool {
	paused, err := rd.IsPaused(ctx, d.ID)
	if err != nil || !paused || !c.restable(ctx, rd, d) {
		return false
	}
	rd.SetRest(d.ID, r)
	log.Printf("idlepause: stopping %s %s", d.ID, why)
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), restStopTimeout)
	defer cancel()
	if err := c.Desktops.Stop(sctx, d.ID); err != nil {
		rd.ClearRest(d.ID)
		log.Printf("idlepause: stopping %s: %v", d.ID, err)
		return false
	}
	return true
}

// reservedMB is the memory starts let through lately are still taken to use, but desktop except's own; expired ones
// are dropped. Under roomMu.
func (c *Checker) reservedMB(now time.Time, except string) int64 {
	var sum int64
	kept := c.reserved[:0]
	for _, r := range c.reserved {
		if now.Before(r.until) {
			kept = append(kept, r)
			if r.id != except {
				sum += r.mb
			}
		}
	}
	c.reserved = kept
	return sum
}

// reserve counts mb against the available memory for reserveFor, for desktop id's start: it replaces an earlier one
// of the same desktop, whose boot a restart ended. Found live (2026-11-08): a practice desktop restarted between the
// cases of a learning curve, every few seconds, piled up a reservation per start and was refused its fourth. Under
// roomMu.
func (c *Checker) reserve(id string, mb int64, now time.Time) {
	kept := c.reserved[:0]
	for _, r := range c.reserved {
		if r.id != id {
			kept = append(kept, r)
		}
	}
	c.reserved = append(kept, reservation{id: id, mb: mb, until: now.Add(reserveFor)})
}

// roundMinutes writes d as "30 minutes".
func roundMinutes(d time.Duration) string {
	m := int(d.Round(time.Minute) / time.Minute)
	if m == 1 {
		return "1 minute"
	}
	return fmt.Sprintf("%d minutes", m)
}
