package skills

import (
	"strings"
	"testing"
)

// A question for names the application lists is held once when a listed window shows that list - a fact of the
// listing, never of the question's words (N-W3-3): the same question goes to the person when no listed screen shows it.
func TestAQuestionForNamesAListOnTheScreenShowsIsPushedBack(t *testing.T) {
	asks := []string{
		"Please confirm the exact ledger names to use for 5% sales and tax in this client's books.",
		"What are the correct account names for the two tax lines?",
		"Can you confirm which categories the system uses for these?",
	}
	others := []string{
		"Is this invoice cancelled? The chat says the customer returned it.",
		"The bill shows two rates - which rule applies to the service line?",
	}
	answers := "Screen 1280x720. Active window: \"Ledgers | Books\"\n" +
		`e1 heading "Chart of Accounts"` + "\n" +
		`e2 row "Ledger | Account | Category"` + "\n" +
		`e3 row "Sales 5% | Sales Accounts | Income"` + "\n" +
		`e4 row "Output Tax 5% | Duties and Taxes | Liability"`
	silent := "Screen 1280x720. Active window: \"Invoice 4417 | Books\"\n" +
		`e1 heading "Invoice 4417"` + "\n" +
		`e2 row "Item | Qty | Rate | Amount"` + "\n" +
		`e3 row "Ceramic mug | 4 | 250.00 | 1,000.00"`
	for _, q := range asks {
		if got := listOnScreen(q, []string{silent, answers}); !strings.Contains(got, "Sales 5% | Sales Accounts | Income") {
			t.Errorf("not pushed back with the list that answers it: %q got %q", q, got)
		}
		if got := listOnScreen(q, []string{silent}); got != "" {
			t.Errorf("pushed back with no list that answers it: %q got %q", q, got)
		}
		if got := listOnScreen(q, nil); got != "" {
			t.Errorf("pushed back with nothing listed: %q got %q", q, got)
		}
	}
	for _, q := range others {
		if got := listOnScreen(q, []string{silent, answers}); got != "" {
			t.Errorf("pushed back wrongly: %q got %q", q, got)
		}
	}
	// A field's own drop-down, open on the screen, answers too; a column of figures or dates names nothing.
	dropdown := `e9 combobox "Category" [expanded]` + "\n" + `e10 option "Income"` + "\n" + `e11 option "Liability"` + "\n" + `e12 button "Save"`
	if got := listOnScreen(asks[2], []string{dropdown}); got != dropdown[:strings.LastIndex(dropdown, "\n")] {
		t.Errorf("drop-down not quoted: %q", got)
	}
	if got := listOnScreen("Which rate is correct for the third line?", []string{silent}); got != "" {
		t.Errorf("a column of figures held a question: %q", got)
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
