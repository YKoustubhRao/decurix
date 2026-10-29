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

func TestASourceWithBracketsStillGivesItsPoints(t *testing.T) {
	ledger := "1 record(s) declared in this task:\n" +
		"- 0195 (from /home/h/Downloads/Anitha (Solar) 0195.pdf; scope Nov): declared, not saved (no save seen)\n"
	issues := recordLedgerIssues(ledger)
	if len(issues) != 1 || !strings.Contains(issues[0].What, "record 0195 was declared") {
		t.Fatalf("a source holding brackets lost its point: %+v", issues)
	}
	if !ledgerSources(ledger)["anitha (solar) 0195.pdf"] {
		t.Fatalf("source not read: %v", ledgerSources(ledger))
	}
}

func TestReadFilesNoRecordCameFromArePointsWhenTheWorkDeclaresRecords(t *testing.T) {
	st := &operatorState{RecordLedger: "1 record(s) declared in this task:\n" +
		"- B2 (from /home/h/Downloads/B-0201 REVISED.pdf): saved and read back\n"}
	for _, n := range []string{"B-0199.pdf", "B-0200.pdf", "B-0201.pdf", "B-0201 REVISED.pdf"} {
		st.addUnit("document", "/home/h/Downloads/"+n, "Bill to Same Buyer GSTIN 29AAAAA0000A1Z5 total 1000")
	}
	st.Written = []string{"29AAAAA0000A1Z5", "1000"} // the one entry's values, which every bill shares
	issues := st.coverageIssues("Entered B-0201 REVISED. B-0201.pdf is the earlier version, replaced by the revised bill.")
	var named []string
	for _, i := range issues {
		named = append(named, i.What)
	}
	got := strings.Join(named, "\n")
	if !strings.Contains(got, "B-0199.pdf") || !strings.Contains(got, "B-0200.pdf") {
		t.Fatalf("bills read and never entered were not raised: %v", named)
	}
	if strings.Contains(got, "B-0201.pdf (") || strings.Contains(got, "REVISED") {
		t.Fatalf("a bill entered, or one the report sets aside with its reason, was raised: %v", named)
	}
}

func TestARateLineThatFitsNoAmountIsAMisreadFigure(t *testing.T) {
	ledger := "2 record(s) declared in this task:\n" +
		"- VAI-3307 (from /home/h/Downloads/IMG-0022.jpg; scope Nov): saved and read back\n" +
		"    No.: VAI-3307 [source; read back: seen]\n" +
		"    Particulars 1: Sales@5% [screen; read back: seen]\n" +
		"    Amount 1: 32700.00 [source; read back: seen]\n" +
		"    Particulars 2: CGST@2.5% [source; read back: seen]\n" +
		"    Amount 2: 737.50 [source; read back: seen]\n" +
		"    Particulars 3: SGST@2.5% [source; read back: seen]\n" +
		"    Amount 3: 737.50 [source; read back: seen]\n" +
		"- VAI-3308 (from /home/h/Downloads/VAI-3308.docx): saved and read back\n" +
		"    Particulars 1: Sales@18% [screen; read back: seen]\n" +
		"    Amount 1: 1,48,000.00 [source; read back: seen]\n" +
		"    Particulars 2: IGST@18% [source; read back: seen]\n" +
		"    Amount 2: 26,640.00 [source; read back: seen]\n" +
		"    Particulars 3: Discount 10% [source; read back: seen]\n" +
		"    Amount 3: 14,800.00 [source; read back: seen]\n"
	issues := rateMismatchIssues(ledger)
	if len(issues) != 2 {
		t.Fatalf("want the two misread tax lines of VAI-3307 only, got %d: %+v", len(issues), issues)
	}
	for _, i := range issues {
		if !strings.Contains(i.What, "VAI-3307") || !strings.Contains(i.What, "29500.00") || !i.Advisory {
			t.Fatalf("point does not name the record and the base it implies, or is not advisory: %+v", i)
		}
	}
}
