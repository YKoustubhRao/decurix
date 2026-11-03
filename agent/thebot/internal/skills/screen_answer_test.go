package skills

import "testing"

func TestAQuestionForNamesTheApplicationListsIsPushedBack(t *testing.T) {
	for _, q := range []string{
		"Please confirm the exact ledger names to use for 5% sales and tax in this client's books.",
		"What are the correct account names for the two tax lines?",
		"Can you confirm which categories the system uses for these?",
	} {
		if !asksAppNamesRe.MatchString(q) {
			t.Errorf("not pushed back: %q", q)
		}
	}
	for _, q := range []string{
		"Is this invoice cancelled? The chat says the customer returned it.",
		"The bill shows two rates - which rule applies to the service line?",
	} {
		if asksAppNamesRe.MatchString(q) {
			t.Errorf("pushed back wrongly: %q", q)
		}
	}
}

// Whose a review point is comes from the reviewer's settle, never from words in the point (review T-18): "recheck"
// is the worker's, "person" the person's, however it is worded.
func TestAPointGoesWhereItsSettleSaysAndAHelpToContinueIsPushedBack(t *testing.T) {
	st := &operatorState{}
	points := []reviewIssue{
		{What: "Bill 457 was never saved - the ledger shows no save for it", Settle: "recheck"},
		{What: "Two of the six bills were left out of the register", Settle: "person"},
		{What: "The party on bill 1188 is matched by name only", Settle: "recheck"},
	}
	work, person := st.workPoints(points), st.notWorkPoints(points)
	if len(work) != 2 || work[0].What != points[0].What || work[1].What != points[2].What {
		t.Errorf("worker's points = %+v", work)
	}
	found := false
	for _, p := range person {
		found = found || p.What == points[1].What
	}
	if !found {
		t.Errorf("a point the reviewer gave the person did not reach them: %+v", person)
	}
	for _, q := range []string{
		"I need the current command outputs to continue. Please resend the task in command mode, or let me continue with commands.",
		"Can you give me permission to run the remaining steps?",
		"I need one new command turn to do the required rechecks before I can truthfully report.",
	} {
		if !asksToContinueRe.MatchString(q) {
			t.Errorf("not pushed back: %q", q)
		}
	}
	if asksToContinueRe.MatchString("Is bill 3309 cancelled, or should I enter it?") {
		t.Errorf("a real question pushed back")
	}
}
