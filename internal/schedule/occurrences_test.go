// Tests for the occurrence ledger: an occurrence is sent once, retried while it never reached its desktop, settled
// from the task records after a crash, and a stretch nobody ran is said.
package schedule

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// dailySchedule opens a store at 08:00 on 15 Sep 2026 holding one schedule due daily at 09:00 UTC.
func dailySchedule(t *testing.T) (*Store, Schedule) {
	t.Helper()
	store := newTestStore(t, time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC))
	sch, err := store.Create("bordu", "file the return", "0 9 * * *", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	return store, sch
}

// checkingSubmitter is a fakeSubmitter that also answers whether the task records hold a run of the schedule.
type checkingSubmitter struct {
	fakeSubmitter
	ran    bool
	ranErr error
}

func (c *checkingSubmitter) ScheduleRanSince(string, time.Time) (bool, error) { return c.ran, c.ranErr }

// A desktop that cannot be reached at 09:00 gets the 09:00 run at 09:05, 09:20 and 09:50; the run that gets through
// still says it is the 09:00 run, and the ledger keeps one row with its tries.
func TestADailyRunIsRetriedUntilItReachesItsDesktop(t *testing.T) {
	store, sch := dailySchedule(t)
	sub := &fakeSubmitter{err: errors.New("desktop did not start")}
	clock := time.Date(2026, 9, 15, 9, 0, 0, 0, time.UTC)
	runner := &Runner{Store: store, Submitter: sub, now: func() time.Time { return clock }}

	for _, wait := range []time.Duration{0, 5 * time.Minute, 15 * time.Minute} {
		clock = clock.Add(wait)
		runner.tick(context.Background())
	}
	got := store.List("")[0]
	if !got.NextRunAt.Equal(time.Date(2026, 9, 15, 9, 50, 0, 0, time.UTC)) || !got.LastRunAt.IsZero() {
		t.Fatalf("after three failed tries: %+v", got)
	}

	sub.mu.Lock()
	sub.err = nil
	sub.mu.Unlock()
	clock = clock.Add(30 * time.Minute)
	runner.tick(context.Background())

	got = store.List("")[0]
	if got.LastError != "" || !got.RetryOccurrence.IsZero() || !got.NextRunAt.Equal(time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("after the run got through: %+v", got)
	}
	if last := sub.calls[len(sub.calls)-1].content; !strings.Contains(last, "TIME:{2026-09-15T09:00:00Z}") {
		t.Fatalf("the retry did not say which run it is: %q", last)
	}
	runs, err := store.Runs(sch.ID, 0)
	if err != nil || len(runs) != 1 || runs[0].State != RunSent || runs[0].Attempts != 4 {
		t.Fatalf("ledger = %+v, %v", runs, err)
	}
}

// After the last try the run is given up, with its reason, and the schedule moves to its next occurrence.
func TestARunIsGivenUpAfterTheLastTry(t *testing.T) {
	store, sch := dailySchedule(t)
	sub := &fakeSubmitter{err: errors.New("desktop did not start")}
	clock := time.Date(2026, 9, 15, 9, 0, 0, 0, time.UTC)
	runner := &Runner{Store: store, Submitter: sub, now: func() time.Time { return clock }}
	for _, wait := range []time.Duration{0, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute} {
		clock = clock.Add(wait)
		runner.tick(context.Background())
	}
	got := store.List("")[0]
	if !got.NextRunAt.Equal(time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)) || !strings.Contains(got.LastError, "did not reach the desktop") {
		t.Fatalf("after the last try: %+v", got)
	}
	runs, _ := store.Runs(sch.ID, 0)
	if len(runs) != 1 || runs[0].State != RunFailed || runs[0].Attempts != 4 {
		t.Fatalf("ledger = %+v", runs)
	}
}

// A busy desktop during a retry keeps the retry's tries.
func TestABusyDesktopDuringARetryKeepsItsTries(t *testing.T) {
	store, sch := dailySchedule(t)
	sub := &fakeSubmitter{err: errors.New("desktop did not start")}
	clock := time.Date(2026, 9, 15, 9, 0, 0, 0, time.UTC)
	runner := &Runner{Store: store, Submitter: sub, now: func() time.Time { return clock }}
	runner.tick(context.Background())

	sub.mu.Lock()
	sub.err = busyErr{}
	sub.mu.Unlock()
	clock = clock.Add(5 * time.Minute)
	runner.tick(context.Background())

	runs, _ := store.Runs(sch.ID, 0)
	if len(runs) != 1 || runs[0].State != RunRetrying || runs[0].Attempts != 1 {
		t.Fatalf("ledger after a busy retry = %+v", runs)
	}
	if got := store.List("")[0]; !got.RetryOccurrence.Equal(time.Date(2026, 9, 15, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("retry lost: %+v", got)
	}
}

// interrupted leaves the 09:00 occurrence written down as being sent, as a crash between the send and the ledger
// would, and returns a runner of a control plane that started at start.
func interrupted(t *testing.T, sub Submitter, start time.Time) (*Store, Schedule, *Runner, *time.Time) {
	t.Helper()
	store, sch := dailySchedule(t)
	occ := time.Date(2026, 9, 15, 9, 0, 0, 0, time.UTC)
	if _, _, err := store.beginRun(sch.ID, occ, occ); err != nil {
		t.Fatal(err)
	}
	clock := start
	runner := &Runner{Store: store, Submitter: sub, now: func() time.Time { return clock }}
	runner.tick(context.Background()) // first tick: within the grace, nothing is decided
	return store, sch, runner, &clock
}

// An occurrence a crash cut short is not sent again when the task records hold it.
func TestAnInterruptedRunTheRecordsHoldIsNotSentAgain(t *testing.T) {
	sub := &checkingSubmitter{ran: true}
	start := time.Date(2026, 9, 15, 9, 1, 0, 0, time.UTC)
	store, sch, runner, clock := interrupted(t, sub, start)
	if len(sub.calls) != 0 {
		t.Fatalf("sent within the reconcile grace: %v", sub.calls)
	}
	*clock = start.Add(reconcileGrace)
	runner.tick(context.Background())
	if len(sub.calls) != 0 {
		t.Fatalf("sent a run the records hold: %v", sub.calls)
	}
	got := store.List("")[0]
	if !got.NextRunAt.Equal(time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)) || got.LastError != "" {
		t.Fatalf("not advanced: %+v", got)
	}
	runs, _ := store.Runs(sch.ID, 0)
	if runs[0].State != RunSent {
		t.Fatalf("ledger = %+v", runs)
	}
}

// One the records do not hold is sent now; one nothing can settle is marked unconfirmed and not sent again.
func TestAnInterruptedRunIsSentOnlyWhenTheRecordsSayItNeverArrived(t *testing.T) {
	start := time.Date(2026, 9, 15, 9, 1, 0, 0, time.UTC)

	sub := &checkingSubmitter{ran: false}
	_, _, runner, clock := interrupted(t, sub, start)
	*clock = start.Add(reconcileGrace)
	runner.tick(context.Background())
	if len(sub.calls) != 1 {
		t.Fatalf("calls = %v, want the run sent once", sub.calls)
	}

	for _, s := range []Submitter{&checkingSubmitter{ranErr: errors.New("records unreadable")}, &fakeSubmitter{}} {
		store, sch, runner, clock := interrupted(t, s, start)
		*clock = start.Add(reconcileGrace)
		runner.tick(context.Background())
		if calls := callsOf(s); calls != 0 {
			t.Fatalf("%T: sent a run nothing could settle", s)
		}
		got := store.List("")[0]
		if !strings.Contains(got.LastError, "was not sent again") || !got.NextRunAt.Equal(time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)) {
			t.Fatalf("%T: %+v", s, got)
		}
		if runs, _ := store.Runs(sch.ID, 0); runs[0].State != RunUnconfirmed {
			t.Fatalf("%T: ledger = %+v", s, runs)
		}
	}
}

// callsOf counts a fake's submits.
func callsOf(s Submitter) int {
	switch f := s.(type) {
	case *checkingSubmitter:
		return len(f.calls)
	case *fakeSubmitter:
		return len(f.calls)
	}
	return -1
}

// Three days away: the oldest due occurrence runs once, and the three after it are one "missed" row and a note.
func TestAStretchNobodyRanIsCountedOnce(t *testing.T) {
	store, sch := dailySchedule(t)
	sub := &fakeSubmitter{}
	clock := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	runner := &Runner{Store: store, Submitter: sub, now: func() time.Time { return clock }}
	runner.tick(context.Background())

	if len(sub.calls) != 1 || !strings.Contains(sub.calls[0].content, "TIME:{2026-09-15T09:00:00Z}") {
		t.Fatalf("calls = %v", sub.calls)
	}
	got := store.List("")[0]
	if !strings.HasPrefix(got.Missed, "3 more run(s)") || !got.NextRunAt.Equal(time.Date(2026, 9, 19, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("after the catch-up run: %+v", got)
	}
	runs, _ := store.Runs(sch.ID, 0)
	if len(runs) != 2 {
		t.Fatalf("ledger = %+v, want the run and one missed row", runs)
	}
	var missed bool
	for _, r := range runs {
		missed = missed || (r.State == RunMissed && r.Occurrence.Equal(time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)))
	}
	if !missed {
		t.Fatalf("no missed row from 16 Sep: %+v", runs)
	}

	// The next run on time clears the note.
	clock = time.Date(2026, 9, 19, 9, 0, 0, 0, time.UTC)
	runner.tick(context.Background())
	if got := store.List("")[0]; got.Missed != "" {
		t.Fatalf("missed note kept after an on-time run: %q", got.Missed)
	}
}

// Turning a schedule off gives up a run waiting for another try; turned on, it starts from its next occurrence.
func TestTurningAScheduleOffGivesUpItsRetry(t *testing.T) {
	store, sch := dailySchedule(t)
	sub := &fakeSubmitter{err: errors.New("desktop did not start")}
	clock := time.Date(2026, 9, 15, 9, 0, 0, 0, time.UTC)
	runner := &Runner{Store: store, Submitter: sub, now: func() time.Time { return clock }}
	runner.tick(context.Background())

	if err := store.SetEnabled(sch.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := store.SetEnabled(sch.ID, true); err != nil {
		t.Fatal(err)
	}
	got := store.List("")[0]
	// The store's clock reads 08:00, so its next occurrence is 09:00 that day - not the 09:05 retry.
	if !got.RetryOccurrence.IsZero() || !got.NextRunAt.Equal(time.Date(2026, 9, 15, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("a paused retry came back: %+v", got)
	}
	if runs, _ := store.Runs(sch.ID, 0); runs[0].State != RunFailed || !strings.Contains(runs[0].Detail, "turned off") {
		t.Fatalf("ledger = %+v", runs)
	}
}
