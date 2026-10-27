package skills

import (
	"strings"
	"testing"
)

const sampleLedger = `3 record(s) declared in this task:
- 1188 (from /home/h/PC/Clients/A/7. OCTOBER/1188 Bill.docx; scope Example Client 2026-27): saved and read back
    No.: 1188 [source; read back: seen]
    Amount 1: 65000.00 [source; read back: seen]
- 1189 (from /home/h/PC/Clients/A/7. OCTOBER/1189 Bill.docx): saved; not read back: Party, Amount 1
    Party: Some Buyer [source; read back: ABSENT]
    Amount 1: 1000 [source; read back: ABSENT]
- 1190 (from /home/h/PC/Clients/A/7. OCTOBER/1190 Bill.docx): declared, not saved (last save blocked: No. holds '1' where the record says '1190')
    No.: 1190 [source; read back: -]`

func TestTheLedgerDecidesItsOwnPoints(t *testing.T) {
	issues := recordLedgerIssues(sampleLedger)
	if len(issues) != 2 {
		t.Fatalf("issues = %d: %+v", len(issues), issues)
	}
	if !strings.Contains(issues[0].What, "record 1189 was saved, but the page did not show these declared values: Party, Amount 1") {
		t.Errorf("absent values: %q", issues[0].What)
	}
	if !strings.Contains(issues[1].What, "record 1190 was declared from its document but never saved") ||
		!strings.Contains(issues[1].Why, "last save blocked") {
		t.Errorf("never saved: %+v", issues[1])
	}
	if got := recordLedgerIssues("No record declared in this task (gui record KEY --from FILE --fields -)."); len(got) != 0 {
		t.Errorf("an empty ledger decides nothing: %+v", got)
	}
}

func TestTheReviewerIsShownTheLedger(t *testing.T) {
	st := &operatorState{Request: "enter the bills", RecordLedger: sampleLedger}
	facts := st.reviewFacts("done", nil, "")
	if !strings.Contains(facts, "THE RECORD LEDGER") || !strings.Contains(facts, "1189 (from") {
		t.Fatalf("the ledger is not among the facts:\n%s", facts)
	}
}

func TestAQuestionTheScreenAnswersIsSentBackOnce(t *testing.T) {
	listings := []string{
		"== window \"Chart of Accounts\" ==\ne12 row \"Coastal Builders | Sundry Debtors | 29AAGFC5512K1ZC | Karnataka\"\ne13 row \"Hebri Agro | Sundry Debtors | 29AAHFH2231Q1Z8\"",
	}
	got := answeredOnScreen(`Which ledger holds the tax number 29AAGFC5512K1ZC? I cannot find it.`, listings)
	if !strings.Contains(got, "Coastal Builders") || strings.Contains(got, "Hebri") {
		t.Fatalf("the line holding the code: %q", got)
	}
	got = answeredOnScreen(`Is "Sundry Debtors" the right group for a customer?`, listings)
	if got == "" {
		t.Fatal("a quoted phrase on the screen is answered there")
	}
	if got := answeredOnScreen("Which registration type should I use for a customer without a tax number?", listings); got != "" {
		t.Fatalf("nothing quoted, nothing to show: %q", got)
	}
	if got := answeredOnScreen(`Should I use "Sundry Creditors"?`, listings); got != "" {
		t.Fatalf("a phrase the screen does not show: %q", got)
	}
}
