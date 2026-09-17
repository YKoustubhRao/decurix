package schedule

import (
	"context"
	"errors"
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

func (f *fakeSubmitter) Submit(_ context.Context, desktopID, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, struct{ desktopID, content string }{desktopID, content})
	return f.err
}

func TestTickFiresDueScheduleWithTimeField(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 30, 0, 0, time.UTC)
	store := newTestStore(t, now)

	if _, err := store.Create("bordu", "check the mail", "*/10 * * * *"); err != nil {
		t.Fatal(err)
	}

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

func TestTickAdvancesEvenWhenSubmitFails(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 30, 0, 0, time.UTC)
	store := newTestStore(t, now)

	sch, err := store.Create("bordu", "check the mail", "*/10 * * * *")
	if err != nil {
		t.Fatal(err)
	}

	submitter := &fakeSubmitter{err: errors.New("desktop unreachable")}

	firedAt := time.Date(2026, 9, 15, 12, 40, 0, 0, time.UTC)
	runner := &Runner{Store: store, Submitter: submitter, now: func() time.Time { return firedAt }}

	runner.tick(context.Background())

	after := store.List("")
	if len(after) != 1 || after[0].ID != sch.ID {
		t.Fatalf("List = %v", after)
	}

	if !after[0].LastRunAt.Equal(firedAt) {
		t.Fatalf("LastRunAt = %v, want %v (a failed submit still consumes this run)", after[0].LastRunAt, firedAt)
	}

	if !after[0].NextRunAt.After(firedAt) {
		t.Fatalf("NextRunAt = %v, want something after %v", after[0].NextRunAt, firedAt)
	}
}

func TestTickDoesNotFireBeforeDue(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 30, 0, 0, time.UTC)
	store := newTestStore(t, now)

	if _, err := store.Create("bordu", "check the mail", "*/10 * * * *"); err != nil {
		t.Fatal(err)
	}

	submitter := &fakeSubmitter{}

	runner := &Runner{Store: store, Submitter: submitter, now: func() time.Time { return now.Add(2 * time.Minute) }}
	runner.tick(context.Background())

	submitter.mu.Lock()
	defer submitter.mu.Unlock()

	if len(submitter.calls) != 0 {
		t.Fatalf("calls = %v, want none yet", submitter.calls)
	}
}
