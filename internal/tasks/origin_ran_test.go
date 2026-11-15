// Tests for RanFromOrigin: what a schedule asks to settle an occurrence a crash cut short.
package tasks

import (
	"testing"
	"time"
)

// A schedule's request is found by its kind and id, from the time it was opened; another schedule's, or one before
// since, is not.
func TestRanFromOriginFindsTheSchedulesOwnRun(t *testing.T) {
	c := newClock()
	s := openStore(t, c)
	box := worker("box", "thebot")
	route, err := s.Plan(box.ID, "file the return")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit(route, box, "file the return", Origin{Kind: OriginSchedule, Ref: "sched-a"}); err != nil {
		t.Fatal(err)
	}
	opened := c.now()

	for _, tc := range []struct {
		ref   string
		since time.Time
		want  bool
	}{
		{"sched-a", opened.Add(-time.Minute), true},
		{"sched-a", opened, true},
		{"sched-a", opened.Add(time.Minute), false},
		{"sched-b", opened.Add(-time.Minute), false},
	} {
		got, err := s.RanFromOrigin(OriginSchedule, tc.ref, tc.since)
		if err != nil || got != tc.want {
			t.Errorf("RanFromOrigin(%s, %v) = %v, %v; want %v", tc.ref, tc.since, got, err, tc.want)
		}
	}
	if got, _ := s.RanFromOrigin(OriginMail, "sched-a", opened.Add(-time.Minute)); got {
		t.Error("another origin kind with the same ref was taken as the schedule's run")
	}
}
