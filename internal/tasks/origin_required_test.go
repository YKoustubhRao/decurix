package tasks

import (
	"errors"
	"testing"
)

// A practice task is never counted as production (review CP-B-2 R1): by its origin, and by its task type when an
// origin says otherwise.
func TestAPracticeTaskIsNotProduction(t *testing.T) {
	for name, origin := range map[string]Origin{
		"named practice":          {Kind: OriginPractice, Ref: "run/c1"},
		"typed practice, unknown": {Kind: OriginUnknown},
	} {
		s := openStore(t, newClock())
		route := NewRoute()
		if _, err := s.CommitTyped(route, worker("practice-r1", "practice"), "Practice (ordinary): x", TypePractice, origin); err != nil {
			t.Fatal(err)
		}
		yes := true
		outcome(t, s, "practice-r1", route.TaskID, Outcome{Kind: OutcomeDone, Reply: "Practice case c1 passed", Checked: &yes})
		rates, _ := s.PassRates()
		for _, r := range rates {
			if r.OriginClass == ClassProduction && r.CheckedDone > 0 {
				t.Fatalf("%s: a practice case is counted as production: %+v", name, r)
			}
		}
	}
}

// An execution offered with an origin that names no kind is refused, by every commit; none is recorded.
func TestAnOriginWithNoKindIsRefused(t *testing.T) {
	s := openStore(t, newClock())
	box := worker("box", "thebot")
	if _, err := s.Commit(NewRoute(), box, "file the bills", Origin{}); !errors.Is(err, ErrNoOrigin) {
		t.Fatalf("Commit with a zero origin = %v", err)
	}
	if _, _, err := s.CommitReply(NewRoute(), box, "file the bills", false, ReplyBy{}, Origin{Ref: "x"}); !errors.Is(err, ErrNoOrigin) {
		t.Fatalf("CommitReply with no kind = %v", err)
	}
	if _, err := s.CommitTyped(NewRoute(), worker("practice-r1", "practice"), "x", TypePractice, Origin{}); !errors.Is(err, ErrNoOrigin) {
		t.Fatalf("CommitTyped with a zero origin = %v", err)
	}
	if list, _ := s.List(Filter{}); len(list) != 0 {
		t.Fatalf("%d tasks recorded", len(list))
	}
	if _, err := s.Commit(NewRoute(), box, "file the bills", Origin{Kind: OriginUnknown}); err != nil {
		t.Fatalf("an origin named unknown on purpose is stored (and logged): %v", err)
	}
}
