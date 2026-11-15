package schedule

import (
	"context"
	"errors"
	"github.com/YKoustubhRao/decurix/internal/tasks"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSubmitter is a Submitter a test can script and inspect.
type fakeSubmitter struct {
	mu    sync.Mutex
	calls []struct{ desktopID, content string }
	err   error
}

func (f *fakeSubmitter) Submit(_ context.Context, desktopID, content string, _ tasks.Origin) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, struct{ desktopID, content string }{desktopID, content})
	return f.err
}

// tenMinuteSchedule opens a store at 12:30 holding one schedule due every ten minutes.
func tenMinuteSchedule(t *testing.T) (*Store, Schedule) {
	t.Helper()

	store := newTestStore(t, time.Date(2026, 9, 15, 12, 30, 0, 0, time.UTC))

	sch, err := store.Create("bordu", "check the mail", "*/10 * * * *", "UTC")
	if err != nil {
		t.Fatal(err)
	}

	return store, sch
}

func TestTickFiresDueScheduleWithTimeField(t *testing.T) {
	store, _ := tenMinuteSchedule(t)

	submitter := &fakeSubmitter{}

	firedAt := time.Date(2026, 9, 15, 12, 40, 0, 0, time.UTC)
	runner := &Runner{Store: store, Submitter: submitter, now: func() time.Time { return firedAt }}

	runner.tick(context.Background())

	submitter.mu.Lock()
	defer submitter.mu.Unlock()

	if len(submitter.calls) != 1 {
		t.Fatalf("calls = %v, want 1", submitter.calls)
	}

	call := submitter.calls[0]
	if call.desktopID != "bordu" {
		t.Fatalf("desktopID = %q, want bordu", call.desktopID)
	}

	if !strings.HasPrefix(call.content, "check the mail\n\nTIME:{") {
		t.Fatalf("content = %q, want it to start with the request text and a TIME field", call.content)
	}

	if !strings.Contains(call.content, "TIME:{2026-09-15T12:40:00Z}") {
		t.Fatalf("content = %q, want it to carry the trigger time", call.content)
	}
}

// A run that did not reach its desktop is tried again 5 minutes later, still as the same occurrence; when the next
// try would come after the schedule's next occurrence, it is given up and the schedule moves on with the reason.
func TestAFailedSubmitIsRetriedThenConsumed(t *testing.T) {
	store, sch := tenMinuteSchedule(t)

	submitter := &fakeSubmitter{err: errors.New("desktop unreachable")}

	clock := time.Date(2026, 9, 15, 12, 40, 0, 0, time.UTC)
	runner := &Runner{Store: store, Submitter: submitter, now: func() time.Time { return clock }}

	runner.tick(context.Background())

	got := store.List("")[0]
	if got.ID != sch.ID || !got.LastRunAt.IsZero() || !got.NextRunAt.Equal(clock.Add(5*time.Minute)) ||
		!got.RetryOccurrence.Equal(time.Date(2026, 9, 15, 12, 40, 0, 0, time.UTC)) || !strings.Contains(got.LastError, "trying again at 12:45") {
		t.Fatalf("after the first failed try: %+v", got)
	}

	clock = clock.Add(5 * time.Minute)
	runner.tick(context.Background())

	got = store.List("")[0]
	if !got.LastRunAt.Equal(clock) || !got.NextRunAt.Equal(time.Date(2026, 9, 15, 12, 50, 0, 0, time.UTC)) ||
		!got.RetryOccurrence.IsZero() || !strings.Contains(got.LastError, "did not reach the desktop") {
		t.Fatalf("a try that would pass the next occurrence was not given up: %+v", got)
	}

	submitter.mu.Lock()
	defer submitter.mu.Unlock()
	if len(submitter.calls) != 2 || !strings.Contains(submitter.calls[1].content, "TIME:{2026-09-15T12:40:00Z}") {
		t.Fatalf("calls = %v, want the 12:40 run tried twice", submitter.calls)
	}
}

func TestTickDoesNotFireBeforeDue(t *testing.T) {
	store, _ := tenMinuteSchedule(t)

	submitter := &fakeSubmitter{}

	runner := &Runner{Store: store, Submitter: submitter, now: func() time.Time { return time.Date(2026, 9, 15, 12, 32, 0, 0, time.UTC) }}
	runner.tick(context.Background())

	submitter.mu.Lock()
	defer submitter.mu.Unlock()

	if len(submitter.calls) != 0 {
		t.Fatalf("calls = %v, want none yet", submitter.calls)
	}
}

type busyErr struct{}

func (busyErr) Error() string       { return "busy" }
func (busyErr) DesktopBusy() string { return "the agent is waiting for your reply" }

func TestABusyDesktopHoldsTheRunInsteadOfSkippingOrSendingIt(t *testing.T) {
	store, sch := tenMinuteSchedule(t)

	submitter := &fakeSubmitter{err: busyErr{}}
	clock := time.Date(2026, 9, 15, 12, 40, 0, 0, time.UTC)
	runner := &Runner{Store: store, Submitter: submitter, now: func() time.Time { return clock }}

	runner.tick(context.Background())

	got := store.List("")[0]
	if !got.NextRunAt.Equal(sch.NextRunAt) || !got.LastRunAt.IsZero() {
		t.Fatalf("a held run was advanced: %+v", got)
	}

	if got.Waiting != "the agent is waiting for your reply" || !got.WaitingSince.Equal(clock) {
		t.Fatalf("waiting = %q since %v", got.Waiting, got.WaitingSince)
	}

	// Still busy a tick later: still held, still waiting since the first time.
	clock = clock.Add(20 * time.Second)
	runner.tick(context.Background())

	if got := store.List("")[0]; !got.WaitingSince.Equal(clock.Add(-20 * time.Second)) {
		t.Fatalf("waiting since moved: %v", got.WaitingSince)
	}

	// Free: it runs once, advances past now, and stops waiting.
	submitter.mu.Lock()
	submitter.err = nil
	submitter.mu.Unlock()

	clock = time.Date(2026, 9, 15, 13, 5, 0, 0, time.UTC)
	runner.tick(context.Background())

	got = store.List("")[0]
	if got.Waiting != "" || !got.LastRunAt.Equal(clock) || !got.NextRunAt.Equal(time.Date(2026, 9, 15, 13, 10, 0, 0, time.UTC)) {
		t.Fatalf("after the desktop was free: %+v", got)
	}

	if n := len(submitter.calls); n != 3 {
		t.Fatalf("submit calls = %d, want 3 (two refused, one sent)", n)
	}
}
