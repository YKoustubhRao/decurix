package main

import (
	"testing"

	"brainiac/internal/evals"
)

// The routed path counts every case the router sends to a field: a case of
// another field sent there is a miss unless the model answered it right.
func TestRoutedScoresCountMisroutedCasesAsMisses(t *testing.T) {
	num := evals.Check{Kind: "number", Value: "1"}
	cases := []evals.Case{
		{Domain: "math", Check: num}, {Domain: "math", Check: num}, // right field, answered right
		{Domain: "finance", Check: num}, {Domain: "finance", Check: num}, // misfiled as math, never asked
		{Domain: "law", Check: evals.Check{Kind: "contains", Value: "x"}}, // not routed
	}
	routed := []string{"math", "math", "math", "math", ""}
	got := routedScores(cases, routed, map[int]bool{0: true, 1: true})
	if len(got) != 1 || got[0].field != "math" {
		t.Fatalf("%+v", got)
	}
	s := got[0].score
	if s.N != 4 || s.Correct != 2 || s.Decided != 4 || s.DecidedCorrect != 2 || got[0].misrouted != 2 {
		t.Fatalf("%+v misrouted %d", s, got[0].misrouted)
	}
}
