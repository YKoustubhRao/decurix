package schedule

import (
	"context"
	"errors"
	"github.com/YKoustubhRao/decurix/internal/tasks"
	"strings"
	"testing"
	"time"
)

// Cron fields read the way vixie cron reads them (review 2026-11-03, B-37).
func TestCronFieldsReadAsVixieReadsThem(t *testing.T) {
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, c := range []struct{ expr, after, want string }{
		// "*/2" day-of-month with a weekday: both must hold. 2026-09-14 is a
		// Monday and even, so the next odd Monday is 2026-09-21.
		{"0 9 */2 * 1", "2026-09-14T10:00:00Z", "2026-09-21T09:00:00Z"},
		// "5/15" in the minute field is 5, 20, 35, 50.
		{"5/15 * * * *", "2026-09-14T10:06:00Z", "2026-09-14T10:20:00Z"},
		// 7 is Sunday. 2026-09-20 is a Sunday.
		{"0 9 * * 7", "2026-09-14T10:00:00Z", "2026-09-20T09:00:00Z"},
		// Both restricted: either one (unchanged).
		{"0 9 15 * 1", "2026-09-14T10:00:00Z", "2026-09-15T09:00:00Z"},
	} {
		e, err := Parse(c.expr)
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		got, ok := e.Next(at(c.after))
		if !ok || !got.Equal(at(c.want)) {
			t.Errorf("%s after %s = %v, want %s", c.expr, c.after, got, c.want)
		}
	}
	if _, err := Parse("0 9 * * 8"); err == nil {
		t.Error("weekday 8 was accepted")
	}
}

type failingSubmitter struct{ contents []string }

func (f *failingSubmitter) Submit(_ context.Context, _, content string, _ tasks.Origin) error {
	f.contents = append(f.contents, content)
	return errors.New("desktop did not start")
}

// A run that did not reach its desktop is consumed with a trace, and the TIME
// it carried is its own occurrence, not the tick that noticed it (B-28).
func TestAFailedRunLeavesATraceAndTimeIsTheOccurrence(t *testing.T) {
	store := newTestStore(t, time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	sch, err := store.Create("bordu", "file the return", "0 13 30 9 *", "UTC")
	if err != nil {
		t.Fatal(err)
	}

	sub := &failingSubmitter{}
	// The control plane was down: the tick notices the 13:00 run at 13:07.
	r := &Runner{Store: store, Submitter: sub, now: func() time.Time { return time.Date(2026, 9, 30, 13, 7, 5, 0, time.UTC) }}
	r.tick(context.Background())

	if len(sub.contents) != 1 || !strings.HasSuffix(sub.contents[0], "TIME:{2026-09-30T13:00:00Z}") {
		t.Fatalf("sent %q", sub.contents)
	}

	got := store.List("")[0]
	if got.ID != sch.ID || !strings.Contains(got.LastError, "did not reach the desktop") {
		t.Fatalf("a failed run left no trace: %+v", got)
	}

	// A later run that goes through clears it.
	if err := store.MarkRun(sch.ID, time.Date(2027, 9, 30, 13, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if got := store.List("")[0]; got.LastError != "" {
		t.Fatalf("a run that went through kept the old error: %q", got.LastError)
	}
}
