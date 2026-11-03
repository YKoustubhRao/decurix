package tasks

import (
	"encoding/json"
	"testing"
)

// The attempt is the unit (review CP-B B5): a failed attempt under v3 that was resent and done under v4 is counted
// under v3, a run that ended waiting on a person is a run, an abandoned one is apart, the first attempt is the
// headline, a checked done counts as independent only on code, a ledger or a person, a person's verdict joins only
// the attempt it judged, and practice and eval are classes of their own.
func TestPassRatesCountEveryAttempt(t *testing.T) {
	c := newClock()
	s := openStore(t, c)
	box := worker("box", "thebot")
	yes := true
	v3, v4 := &Skill{Name: "bills", Version: 3}, &Skill{Name: "bills", Version: 4}
	codeDone, _ := ParseVerdict(json.RawMessage(`{"schema":"decurix.verdict/v1","verdict":"done","by":"verify","basis":["code"],` +
		`"checks":[{"id":"files","result":"pass","basis":"code","detail":"","refs":[]}],"evidence":[]}`))
	screenDone, _ := ParseVerdict(json.RawMessage(`{"schema":"decurix.verdict/v1","verdict":"done","by":"verify","basis":["screen"],` +
		`"checks":[{"id":"judge","result":"pass","basis":"screen","detail":"","refs":[]}],"evidence":[]}`))

	first := send(t, s, box, "file the bills")
	outcome(t, s, "box", first.TaskID, Outcome{Kind: OutcomeError, Reply: "could not", Skill: v3})
	if retry := send(t, s, box, "file the bills"); retry.Kind != RouteRetry {
		t.Fatalf("not a retry: %+v", retry)
	}
	outcome(t, s, "box", first.TaskID, Outcome{Kind: OutcomeDone, Reply: "done", Skill: v4, Verdict: codeDone})
	if _, err := s.SetVerdict(first.TaskID, VerdictWrong, "", LocalOwner); err != nil {
		t.Fatal(err)
	}

	second := send(t, s, box, "file the receipts")
	outcome(t, s, "box", second.TaskID, Outcome{Kind: OutcomeDone, Reply: "done", Skill: v4, Verdict: screenDone, Checked: &yes})

	third := send(t, s, box, "file the invoices")
	outcome(t, s, "box", third.TaskID, Outcome{Kind: OutcomeQuery, Reply: "Which month?", Skill: v4})
	if err := s.CloseOpen("box", "the desktop was stopped"); err != nil {
		t.Fatal(err)
	}

	practice := NewRoute()
	if _, err := s.CommitTyped(practice, worker("practice-r1", "practice"), "practice case", TypePractice,
		Origin{Kind: OriginPractice, Ref: "run/case"}); err != nil {
		t.Fatal(err)
	}
	outcome(t, s, "practice-r1", practice.TaskID, Outcome{Kind: OutcomeDone, Reply: "graded", Skill: v4,
		Verdict: GraderVerdict(ByPractice, "pass", "code", "mock", "", nil)})

	rates, err := s.PassRates()
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]PassRate{}
	for _, r := range rates {
		by[r.OriginClass+"/"+r.SkillName+"/"+string(rune('0'+r.SkillVersion))] = r
	}
	if r, ok := by["production/bills/3"]; !ok || r.Runs != 1 || r.CheckedDone != 0 || r.FirstAttemptRuns != 1 ||
		r.PersonWrong != 0 {
		t.Errorf("v3's failed first attempt = %+v (%v)", r, ok)
	}
	r := by["production/bills/4"]
	if r.Runs < 3 || r.CheckedDone != 2 || r.CheckedIndependent != 1 || r.FirstAttemptRuns < 2 || r.FirstAttemptChecked != 1 ||
		r.PersonWrong != 1 || r.WrongButDone != 1 {
		t.Errorf("v4 = %+v", r)
	}
	if p, ok := by["practice/bills/4"]; !ok || p.Runs != 1 || p.CheckedIndependent != 1 {
		t.Errorf("practice = %+v (%v): a practice case is a class of its own", p, ok)
	}
}
