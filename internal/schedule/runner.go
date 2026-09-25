package schedule

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"
)

// Submitter sends a request to a desktop exactly as a human typing it into
// the requests page and pressing Send would: Plan a task route, deliver it
// (starting the desktop first if it is stopped), then Commit the route and
// append it to the chat. Satisfied by *httpapi.Server; narrowed to an
// interface so this package needs no import of net/http or any of httpapi's
// other dependencies, and can be tested with a fake instead of a real server.
type Submitter interface {
	Submit(ctx context.Context, desktopID, content string) error
}

// defaultInterval is how often Run checks for due schedules when Interval is
// unset. Independent of any one schedule's own cadence: a schedule due every
// minute still fires within one Interval of its minute, not one Interval per
// check of that minute specifically.
const defaultInterval = 20 * time.Second

// fireConcurrency bounds how many due schedules tick fires at once, mirroring
// internal/idlepause.Checker's sweepConcurrency. A stopped desktop's Submit
// can block up to httpapi's autoStartTimeout (90s) waiting for it to boot;
// idle-pause being on by default makes that the common case for a scheduled
// desktop, not the exception, so firing schedules one at a time would let one
// slow-to-start desktop hold up every other schedule due in the same tick.
const fireConcurrency = 8

// Runner fires each schedule's request when its cron expression next
// matches.
type Runner struct {
	Store     *Store
	Submitter Submitter
	Interval  time.Duration // defaultInterval if zero

	// now is the test seam - defaults to time.Now().UTC.
	now func() time.Time
}

// Run checks for due schedules every Interval until ctx is done.
//
// A schedule due after the control plane was down for a while (a missed
// restart, say) fires once on the first tick that notices it, not once for
// every minute it missed: MarkRun always advances a schedule to the next
// occurrence strictly after the time it actually ran, never replays the
// occurrences in between, so there is nothing to catch up on beyond that one
// run.
func (r *Runner) Run(ctx context.Context) {
	interval := r.Interval
	if interval <= 0 {
		interval = defaultInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		r.tick(ctx)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *Runner) tick(ctx context.Context) {
	now := r.clock()

	due, err := r.Store.Due(now)
	if err != nil {
		log.Printf("schedule: checking due schedules: %v", err)
		return
	}

	sem := make(chan struct{}, fireConcurrency)
	var wg sync.WaitGroup

	for _, sch := range due {
		wg.Add(1)
		sem <- struct{}{}

		go func(sch Schedule) {
			defer wg.Done()
			defer func() { <-sem }()

			r.fire(ctx, sch, now)
		}(sch)
	}

	wg.Wait()
}

// fire submits sch's request, with a TIME field recording when this run
// triggered appended the same way on every trigger, and advances the
// schedule to its next occurrence regardless of whether the submit itself
// succeeded: a desktop that is unreachable this minute should not turn every
// following tick into a retry of the same stale run - it should just wait for
// the schedule's next one.
//
// The one exception is a desktop that is busy (the Submitter refuses with an
// error that has a DesktopBusy method): the run is held, not sent and not
// skipped, and tried again every tick until the desktop is free. Sent to an
// agent waiting on a person, it would be taken as that person's answer.
func (r *Runner) fire(ctx context.Context, sch Schedule, now time.Time) {
	content := sch.Content + timeField(now)

	err := r.Submitter.Submit(ctx, sch.DesktopID, content)

	var busy interface{ DesktopBusy() string }
	if errors.As(err, &busy) {
		if sch.Waiting == "" {
			log.Printf("schedule: holding %s until %s is free (%s)", sch.ID, sch.DesktopID, busy.DesktopBusy())
		}

		r.Store.NoteWaiting(sch.ID, busy.DesktopBusy(), now)

		return
	}

	if err != nil {
		log.Printf("schedule: firing %s on %s: %v", sch.ID, sch.DesktopID, err)
	}

	if err := r.Store.MarkRun(sch.ID, now); err != nil {
		log.Printf("schedule: advancing %s: %v", sch.ID, err)
	}
}

// timeField renders the marker appended to a schedule's content on every
// trigger, recording when it fired - the one thing the request's own text
// cannot say for itself.
func timeField(t time.Time) string {
	return "\n\nTIME:{" + t.UTC().Format(time.RFC3339) + "}"
}

func (r *Runner) clock() time.Time {
	if r.now != nil {
		return r.now()
	}

	return time.Now().UTC()
}
