package schedule

import (
	"context"
	"errors"
	"github.com/YKoustubhRao/decurix/internal/tasks"
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
	Submit(ctx context.Context, desktopID, content string, origin tasks.Origin) error
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

	// started is when this runner first ticked (startedAt).
	startOnce sync.Once
	started   time.Time
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

	Every(ctx, interval, r.tick)
}

// Every calls fn at once and then every interval until ctx is done.
func Every(ctx context.Context, interval time.Duration, fn func(context.Context)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		fn(ctx)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Concurrently calls fn on every item, at most limit at a time, and returns
// once every call has.
func Concurrently[T any](items []T, limit int, fn func(T)) {
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup

	for _, item := range items {
		wg.Add(1)
		sem <- struct{}{}

		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			fn(item)
		}()
	}

	wg.Wait()
}

func (r *Runner) tick(ctx context.Context) {
	now := r.clock()
	r.startedAt()

	due, err := r.Store.Due(now)
	if err != nil {
		log.Printf("schedule: checking due schedules: %v", err)
		return
	}

	Concurrently(due, fireConcurrency, func(sch Schedule) { r.fire(ctx, sch, now) })
}

// RanChecker is what a Submitter may also offer: whether the task records hold a request this schedule started at or
// after since. It settles an occurrence a crash cut short between the send and the ledger: found, it was sent; not
// found, it never reached the records and is sent again. Without it such an occurrence is never sent again.
type RanChecker interface {
	ScheduleRanSince(scheduleID string, since time.Time) (bool, error)
}

// reconcileGrace is how long after this runner started an occurrence a crash cut short waits before it is settled:
// the control plane settles its own interrupted deliveries at start (httpapi's ReconcileDeliveries), and their task
// records must be in before the schedule asks them.
const reconcileGrace = 2 * time.Minute

// fire runs one due occurrence of sch through its ledger row (occurrences.go), with a TIME field recording which
// occurrence it is appended the same way on every trigger.
//
// The occurrence is written down before it is sent and settled together with the schedule's advance, so a crash
// between the two never sends it twice: a row a crash left "sending" is settled from the task records (RanChecker),
// and when they cannot say, it is marked unconfirmed and not sent again.
//
// A desktop that is busy (the Submitter refuses with an error that has a DesktopBusy method) holds the run - not
// sent and not skipped, tried again every tick until the desktop is free: sent to an agent waiting on a person, it
// would be taken as that person's answer.
//
// A run that did not reach its desktop is tried again after 5, 15 and 30 minutes (retryBackoff) - the control plane
// knows it was not delivered, so a retry cannot do the work twice - and given up, with its reason kept, after the
// last try or when the schedule's next occurrence would come first. Before this a desktop that failed to start at
// 09:00 lost that month's run of a monthly request.
//
// TIME is the occurrence the run is for (the schedule's next run as stored, or the occurrence a retry is for), not
// the moment the tick noticed it: a run held behind a busy desktop, retried, or caught up after the control plane
// was down still says which run it is (review 2026-11-03, B-28). Occurrences that passed while nothing ran them are
// counted into one "missed" row (missedBetween).
func (r *Runner) fire(ctx context.Context, sch Schedule, now time.Time) {
	occurrence := sch.NextRunAt
	retry := !sch.RetryOccurrence.IsZero()
	if retry {
		occurrence = sch.RetryOccurrence
	}
	if occurrence.IsZero() || occurrence.After(now) {
		occurrence = now
	}
	loc := ZoneLocation(sch.Zone)
	occurrence = occurrence.In(loc)

	prev, existed, err := r.Store.beginRun(sch.ID, occurrence, now)
	if err != nil {
		log.Printf("schedule: %v", err)
		return
	}
	if existed && !r.settleInterrupted(sch, occurrence, prev, now) {
		return
	}

	content := sch.Content + timeField(occurrence)
	err = r.Submitter.Submit(ctx, sch.DesktopID, content, tasks.Origin{Kind: tasks.OriginSchedule, Ref: sch.ID})

	var busy interface{ DesktopBusy() string }
	if errors.As(err, &busy) {
		if sch.Waiting == "" {
			log.Printf("schedule: holding %s until %s is free (%s)", sch.ID, sch.DesktopID, busy.DesktopBusy())
		}

		r.Store.holdRun(sch.ID, occurrence, existed && prev.State == RunRetrying)
		r.Store.NoteWaiting(sch.ID, busy.DesktopBusy(), now)

		return
	}

	if err != nil {
		log.Printf("schedule: firing %s on %s: %v", sch.ID, sch.DesktopID, err)
		why := "The run for " + occurrence.Format("2 Jan 15:04 MST") + " did not reach the desktop: " + clip(err.Error())

		attempts := prev.Attempts
		if attempts < len(retryBackoff) {
			retryAt := now.Add(retryBackoff[attempts])
			if next, ok := nextAfter(sch, occurrence); !ok || retryAt.Before(next) {
				why += "; trying again at " + retryAt.In(loc).Format("15:04 MST")
				if err := r.Store.retryRun(sch.ID, occurrence, now, retryAt, why); err != nil {
					log.Printf("schedule: %v", err)
				}
				return
			}
		}

		if err := r.Store.settleRun(sch.ID, occurrence, now, RunFailed, why); err != nil {
			log.Printf("schedule: advancing %s: %v", sch.ID, err)
		}

		return
	}

	if err := r.Store.settleRun(sch.ID, occurrence, now, RunSent, ""); err != nil {
		log.Printf("schedule: advancing %s: %v", sch.ID, err)
	}

	if !retry && !sch.RunOnce {
		if first, last, n := missedBetween(sch.CronExpr, occurrence, now, loc); n > 0 {
			r.Store.noteMissed(sch.ID, first, last, n, now, loc)
		}
	}
}

// settleInterrupted decides an occurrence whose ledger row already existed: a retry goes ahead; one already sent,
// given up or unconfirmed only advances the schedule; one a crash left "sending" is settled from the task records.
// It reports whether the occurrence should be sent now.
func (r *Runner) settleInterrupted(sch Schedule, occurrence time.Time, prev Run, now time.Time) bool {
	switch prev.State {
	case RunRetrying:
		return true
	case RunSending:
	default:
		if err := r.Store.settleRun(sch.ID, occurrence, now, prev.State, prev.Detail); err != nil {
			log.Printf("schedule: advancing %s: %v", sch.ID, err)
		}
		return false
	}

	if now.Sub(r.startedAt()) < reconcileGrace {
		return false // asked again on a later tick, once interrupted deliveries are settled
	}

	checker, ok := r.Submitter.(RanChecker)
	if ok {
		ran, err := checker.ScheduleRanSince(sch.ID, prev.StartedAt)
		if err == nil && ran {
			if err := r.Store.settleRun(sch.ID, occurrence, now, RunSent, "sent before the control plane stopped"); err != nil {
				log.Printf("schedule: advancing %s: %v", sch.ID, err)
			}
			return false
		}
		if err == nil {
			return true // it never reached the records: send it now
		}
	}

	why := "The run for " + occurrence.In(ZoneLocation(sch.Zone)).Format("2 Jan 15:04 MST") +
		" was being sent when the control plane stopped, and its desktop's records cannot say whether it arrived. " +
		"It was not sent again; check the desktop's tasks."
	if err := r.Store.settleRun(sch.ID, occurrence, now, RunUnconfirmed, why); err != nil {
		log.Printf("schedule: advancing %s: %v", sch.ID, err)
	}
	return false
}

// timeField renders the marker appended to a schedule's content on every
// trigger, recording when it fired - the one thing the request's own text
// cannot say for itself. It is RFC3339 in t's own zone (the schedule's), so
// a run at 00:30 in Asia/Kolkata says that day's date, not yesterday's UTC
// one; a UTC schedule still reads "...Z".
func timeField(t time.Time) string {
	return "\n\nTIME:{" + t.Format(time.RFC3339) + "}"
}

// clip bounds a reason kept on a schedule.
func clip(s string) string {
	if r := []rune(s); len(r) > 200 {
		return string(r[:200]) + "..."
	}
	return s
}

// startedAt is when this runner first ticked: occurrences a crash cut short wait reconcileGrace after it.
func (r *Runner) startedAt() time.Time {
	r.startOnce.Do(func() { r.started = r.clock() })
	return r.started
}

func (r *Runner) clock() time.Time {
	if r.now != nil {
		return r.now()
	}

	return time.Now().UTC()
}
