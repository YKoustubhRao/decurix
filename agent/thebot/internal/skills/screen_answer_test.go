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

func TestAPointThatSaysNotDoneIsTheWorkersAndAHelpToContinueIsPushedBack(t *testing.T) {
	for _, w := range []string{
		"The requested task was not completed: December sales bill 431 was not entered as a Sales voucher",
		"Bill 457 was never saved - the ledger shows no save for it",
		"Two of the six bills were left out of the register",
	} {
		if !saysNotDone(w) {
			t.Errorf("not seen as not done: %q", w)
		}
	}
	if saysNotDone("The party on bill 1188 is matched by name only, not by its identifier") {
		t.Errorf("a judgement seen as not done")
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
