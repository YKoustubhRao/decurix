package tasks

import "testing"

// Review CP-B B5: a failed attempt under skill v3, resent and done under v4: v3's failure must be counted somewhere.
func TestPassRatesKeepAFailedAttempt(t *testing.T) {
	c := newClock()
	s := openStore(t, c)
	box := worker("box", "thebot")
	first := send(t, s, box, "file the bills")
	outcome(t, s, "box", first.TaskID, Outcome{Kind: OutcomeError, Reply: "could not", Skill: &Skill{Name: "bills", Version: 3}})
	retry := send(t, s, box, "file the bills")
	if retry.Kind != RouteRetry {
		t.Fatalf("not a retry: %+v", retry)
	}
	yes := true
	outcome(t, s, "box", first.TaskID, Outcome{Kind: OutcomeDone, Reply: "done", Checked: &yes, Skill: &Skill{Name: "bills", Version: 4}})
	rates, err := s.PassRates()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rates {
		if r.SkillVersion == 3 {
			return
		}
	}
	t.Fatalf("v3's failed run is in no row: %+v", rates)
}
