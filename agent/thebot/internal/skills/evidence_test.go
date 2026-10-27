package skills

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// docs/harness-v2.md, Phase 1: what the goal was shown kept as units with their source.

func TestWhatIsReadIsKeptAsUnitsWithTheirSource(t *testing.T) {
	st := &operatorState{}
	st.noteEvidence("office read '/w/06. SEPTEMBER/Invoice_455_Karavali.pdf'", "Invoice No: 455\nHSN 0801  Qty 40\nTaxable 8,000.00")
	st.noteEvidence("office read /w/SALES.xlsx", "[SEPTEMBER] A60=451 B60=26000\n[SEPTEMBER] A61=452 B61=156000\nno figure here")
	st.noteEvidence("python3 sum.py", "('B2B', '0801', '5') 156000\ndone")
	kinds := map[string]int{}
	for _, u := range st.Units {
		kinds[u.Kind]++
	}
	if kinds["document"] != 1 || kinds["row"] != 2 || kinds["computed"] != 1 {
		t.Fatalf("units = %+v", st.Units)
	}
	if st.Units[0].Source != "Invoice_455_Karavali.pdf" || !st.Units[0].holds("8000") || st.Units[0].holds("800") {
		t.Errorf("document unit: %+v", st.Units[0])
	}
}

// Found live (2026-10-23): invoice 455's number with 456's amount.
func TestValuesOfOneRecordFromDifferentSourceRecordsAreFound(t *testing.T) {
	st := &operatorState{Hosted: true}
	st.addUnit("document", "Invoice_455.pdf", "Invoice No 455 Karavali HSN 0801 taxable 8,000.00 CANCELLED")
	st.addUnit("document", "Invoice_456.pdf", "Invoice No 456 Mohammed Ashraf HSN 1006 taxable 2,750.00")
	if note := st.noteRecordWork(`gui type e1 "456" && gui type e2 "2750" && gui click e3`); note != "" {
		t.Errorf("values of one invoice were flagged: %s", note)
	}
	note := st.noteRecordWork(`gui type e1 "455" && gui type e2 "2750" && gui click e3`)
	if !strings.Contains(note, "stand together in no single") || !strings.Contains(note, "Invoice_455.pdf") ||
		!strings.Contains(note, "Invoice_456.pdf") || len(st.RecordFindings) != 1 {
		t.Fatalf("note = %q findings %q", note, st.RecordFindings)
	}
	// A value nobody showed is named as such; a rate or a date never counts.
	if note := st.noteRecordWork(`gui type e1 "455" && gui type e2 "0910" && gui type e3 "18%" && gui type e4 "30-09-2026" && gui click e9`); !strings.Contains(note, `"0910" in nothing you were shown`) {
		t.Errorf("note = %q", note)
	}
	if st.RecordValues != nil {
		t.Error("the record was not closed by the click")
	}
}

func TestADocumentNothingEnteredReflectsIsAGap(t *testing.T) {
	st := &operatorState{ScreenDocs: []string{"IMG_0932.jpg"}}
	st.addUnit("document", "Invoice_451.pdf", "Invoice 451 taxable 26,000.00")
	st.addUnit("document", "Invoice_455.pdf", "Invoice 455 taxable 8,000.00 CANCELLED")
	st.noteWritten(`gui type e4 "26000"`)
	gaps := strings.Join(st.coverageGaps(), "\n")
	if strings.Contains(gaps, "Invoice_451.pdf") || !strings.Contains(gaps, "Invoice_455.pdf (read;") ||
		!strings.Contains(gaps, "IMG_0932.jpg (offered or saved; never read)") {
		t.Fatalf("gaps = %s", gaps)
	}
	st.noteWritten("office update SALES.xlsx - <<'EOF'\n{\"edits\": [{\"sheet\": \"SALES\", \"cell\": \"F62\", \"value\": 8000}]}\nEOF")
	if strings.Contains(strings.Join(st.coverageGaps(), "\n"), "Invoice_455.pdf (read;") {
		t.Error("a value written into a workbook did not count")
	}
}

// Before, once any page had come as a picture, nothing typed was traced at all.
func TestAValueOnlyAPictureCouldHoldIsTypedButKeptAsUncertain(t *testing.T) {
	st := &operatorState{ShownPages: true, Seen: "Invoice 457 Rice 50 kg"}
	if nudge, _ := groundingCheck(st, `gui type e5 "0910"`); nudge != "" {
		t.Fatalf("blocked: %s", nudge)
	}
	if len(st.PictureOnly) != 1 || st.PictureOnly[0] != "0910" {
		t.Fatalf("picture-only = %q", st.PictureOnly)
	}
	if facts := st.reviewFacts("done", nil, ""); !strings.Contains(facts, "VALUES TYPED THAT NO TEXT HOLDS") {
		t.Error("the reviewer was not told")
	}
}

func TestTheVerifierFillsBlanksItIsNotShownAndDisagreementsAreRaised(t *testing.T) {
	listing := `row "B2C (Others) | Karnataka | 5% | 34,750.00 | 868.75" (e12)` + "\n" +
		`row "Documents issued | 451 | 458 | 8 | 0" (e13)` + "\nheading \"GSTR-1 Summary\" (e3)"
	masked, figures := maskedFigures(listing)
	if strings.Contains(masked, "34,750.00") || !strings.Contains(masked, "5%") || !strings.Contains(masked, "(e12)") ||
		len(figures) != 6 {
		t.Fatalf("masked %q figures %v", masked, figures)
	}
	chat := &scriptedChat{replies: []string{`{"v1": "34750", "v2": "868.75", "v3": "451", "v4": "458", "v5": "8", "v6": "1"}`}}
	st := &operatorState{Hosted: true, Request: "file GSTR-1", Listings: []string{listing}}
	issues := (&OperatorSkill{}).verifyCommit(weighRC(chat), st, "")
	if len(issues) != 1 || !strings.Contains(issues[0].What, "shows 0 where an independent working from the sources gives 1") ||
		issues[0].Settle != "recheck" {
		t.Fatalf("issues = %+v", issues)
	}
	if sent := chat.calls[0][1].Content; strings.Contains(sent, "34,750.00") || strings.Contains(sent, "868.75") {
		t.Error("the verifier was shown the figures it was to work out")
	}
}

// Found live (2026-10-23): a phone photo's two lines were written into a workbook as 8,750 and 5,000 for 2,750 and
// 6,000; nothing looked at a value written with office update.
func TestAValueWrittenIntoAFileThatNoTextHoldsIsAFactForTheReviewer(t *testing.T) {
	st := &operatorState{}
	st.noteSeen("Sona masoori rice 1006 50 KGS 55.00 5% 2750.00 | Total 8750.00")
	st.noteWritten("office update book.xlsx - <<'EOF'\n{\"edits\": [{\"sheet\": \"SALES\", \"cell\": \"I33\", \"value\": 2750}, " +
		"{\"sheet\": \"SALES\", \"cell\": \"I34\", \"value\": 5000}]}\nEOF")
	if len(st.UntracedWrites) != 1 || st.UntracedWrites[0] != "5000" {
		t.Fatalf("untraced writes: %v", st.UntracedWrites)
	}
	if facts := st.reviewFacts("done", nil, ""); !strings.Contains(facts, "VALUES WRITTEN INTO FILES THAT NO TEXT") ||
		!strings.Contains(facts, "5000") {
		t.Errorf("the reviewer was not shown it: %s", facts)
	}
}

// Found live (2026-10-24): a phone photo of a sale bill was saved with the others and never entered; the review raised
// other points. A document nothing written reflects, that the report never names, is a point decided in code.
func TestADocumentNothingReflectsAndTheReportDoesNotNameIsAPoint(t *testing.T) {
	st := &operatorState{ScreenDocs: []string{"IMG_0932.jpg", "Bill_456.pdf"}}
	st.addUnit("document", "Bill_456.pdf", "Bill 456 ... 8,000.00")
	st.noteWritten("office update s.xlsx - <<'EOF'\n{\"edits\": [{\"sheet\": \"S\", \"cell\": \"I32\", \"value\": 8000}]}\nEOF")
	issues := st.coverageIssues("Saved the month's bills and entered them in the register.")
	if len(issues) != 1 || !strings.Contains(issues[0].What, "IMG_0932.jpg") || issues[0].Settle != "recheck" {
		t.Fatalf("issues = %+v", issues)
	}
	if got := st.coverageIssues("Left IMG_0932.jpg out: it is another client's bill."); len(got) != 0 {
		t.Errorf("a document the report accounts for was raised: %+v", got)
	}
}

// Found live (2026-10-23): a date picker's "10/23/2026" had its first part checked as a figure.
func TestADateIsOneValueNotThreeFigures(t *testing.T) {
	masked, figures := maskedFigures(`toggle "10/23/2026" (e15)` + "\n" + `row "Total | 34,750.00 | 2026-09-30" (e16)`)
	// The date's line holds no figure to check (it is left out); the other keeps its date whole.
	if len(figures) != 1 || figures["v1"] != "34,750.00" || strings.Contains(masked, "/23/") ||
		!strings.Contains(masked, "2026-09-30") {
		t.Fatalf("masked %q figures %v", masked, figures)
	}
}

// Found live (2026-10-23): the last listing was of an image viewer, and its zoom and pixel figures were put to the
// person before a filing on another window. The verifier checks the window the action acts on.
func TestTheVerifierChecksTheWindowTheActionActsOn(t *testing.T) {
	portal := "Screen 1280x720. Active window: \"File Return | Portal - Chromium\"\n" +
		`row "Documents issued | 451 | 458 | 8 | 0" (e13)`
	viewer := "Screen 1280x720. Active window: \"[IMG_0932] (imported)-1.0 (RGB color 8-bit)\"\n" +
		`textbox "1708" (e1997)` + "\n" + `combobox "18.2 %" (e2014)` + "\n" + `label "37.8 MB" (e2016)`
	chat := &scriptedChat{replies: []string{`{"v1": "451", "v2": "458", "v3": "8", "v4": "Background"}`}}
	st := &operatorState{Hosted: true, Request: "file the return", Listings: []string{portal, viewer}}
	action := `click button "FILE" (e1523) in "File Return | Portal - Chromium": it files something on your behalf`
	issues := (&OperatorSkill{}).verifyCommit(weighRC(chat), st, action)
	sent := chat.calls[0][1].Content
	if strings.Contains(sent, "[v5]") || strings.Contains(sent, "e1997") || !strings.Contains(sent, "(e13)") {
		t.Fatalf("the verifier was not shown the action's own window: %s", sent)
	}
	if strings.Contains(sent, "1280") {
		t.Error("the listing's header was taken for figures")
	}
	if len(issues) != 0 {
		t.Errorf("a figure answered with words was raised: %+v", issues)
	}
	other := &operatorState{Hosted: true, Request: "file the return", Listings: []string{viewer}}
	if got := (&OperatorSkill{}).verifyCommit(weighRC(&scriptedChat{}), other, action); got != nil {
		t.Errorf("with no listing of the action's window, another window was verified: %+v", got)
	}
}

// Found live (2026-10-24): a listing shows every window; the check before a filing worked out a file manager's
// status-bar figures behind the browser.
func TestTheCheckBeforeACommitSeesOnlyTheActionsWindow(t *testing.T) {
	listing := "Screen 1280x720. Active window: \"File Return | Portal - Chromium\"\n" +
		"Open windows: \"06. SEPTEMBER - File Manager\", \"File Return | Portal - Chromium\"\n" +
		"== File Return | Portal - Chromium ==\n" + `row "Tax payable | 22,881.36" (e13)` + "\n" +
		"== Thunar frame \"06. SEPTEMBER - File Manager\" ==\n" + `status "6 files: 343.4 KiB (351,603 bytes) | Free space: 214.0 GiB" (e6767)`
	action := `click button "FILE" (e20) in "File Return | Portal - Chromium": it files something on your behalf`
	st := &operatorState{Listings: []string{listing}}
	got := st.actionListing(action)
	if !strings.Contains(got, "22,881.36") || strings.Contains(got, "KiB") || !strings.Contains(got, "Active window:") {
		t.Fatalf("listing = %q", got)
	}
	_, figures := maskedFigures(got)
	if len(figures) != 1 {
		t.Errorf("figures = %v", figures)
	}
	if whole := windowPart(listing, "Some Other Window"); whole != listing {
		t.Error("a listing with no group of the window was cut")
	}
}

// Found live (2026-10-24): files read in one chained command were "never read" to the record.
func TestEachFileAChainedCommandReadsIsCreditedWithItsOwnPart(t *testing.T) {
	st := &operatorState{ScreenDocs: []string{"Bill_455.pdf", "PB_778.pdf"}}
	out := "Bill_455.pdf: PDF, 1 page(s); pages 1-1.\nBill 455 taxable 8,000.00 CANCELLED\n" +
		"PB_778.pdf: PDF, 1 page(s); pages 1-1.\nPurchase bill 778 cashew 1,20,000.00\n" +
		"BOOK.xlsx: sheets ['ITC']\n[ITC] A5=778 B5=120000\nchecked 2"
	st.noteEvidence(`office read "/w/Bill_455.pdf" 1-5 && office read '/w/PB_778.pdf' && office read /w/BOOK.xlsx && python3 - <<'PY'
print("checked 2")
PY`, out)
	var bill, pb *evidenceUnit
	rows := 0
	for i := range st.Units {
		u := &st.Units[i]
		switch {
		case u.Kind == "document" && u.Source == "Bill_455.pdf":
			bill = u
		case u.Kind == "document" && u.Source == "PB_778.pdf":
			pb = u
		case u.Kind == "row" && u.Source == "BOOK.xlsx":
			rows++
		}
	}
	if bill == nil || pb == nil || rows == 0 {
		t.Fatalf("units = %+v", st.Units)
	}
	if !bill.holds("8000") || bill.holds("120000") || !pb.holds("120000") || pb.holds("8000") {
		t.Errorf("parts mixed: bill %+v pb %+v", bill, pb)
	}
	for _, gap := range st.coverageGaps() {
		if strings.Contains(gap, "never read") {
			t.Errorf("a file read in the chain is still a gap: %s", gap)
		}
	}
}

// Found live (2026-10-24): the report pasted the folder's listing, which named a photo bill never entered, and the
// point was never raised. Named alone it is not accounted for; a reason for leaving it out is.
func TestADocumentNamedOnlyInAListingIsStillAPoint(t *testing.T) {
	st := &operatorState{ScreenDocs: []string{"IMG_0932.jpg"}}
	listing := "Saved in the September folder:\n- Bill_451.pdf\n- IMG_0932.jpg\n- Bill_452.pdf"
	if got := st.coverageIssues(listing); len(got) != 1 || !strings.Contains(got[0].What, "IMG_0932.jpg") {
		t.Fatalf("issues = %+v", got)
	}
	for _, report := range []string{"IMG_0932.jpg is a duplicate of bill 451 - not entered.",
		"Left out IMG_0932.jpg; it is another client's bill.", "IMG_0932.jpg: cancelled by the client, kept on file only."} {
		if got := st.coverageIssues(report); len(got) != 0 {
			t.Errorf("a reason was given and the point raised: %q -> %+v", report, got)
		}
	}
}

// Found live (2026-10-25): a script whose text held "find )" made intoSharedLink loop forever (no word taken, no
// progress), and the soul spun for half an hour with the task stopped.
func TestIntoSharedLinkAlwaysEnds(t *testing.T) {
	done := make(chan string, 1)
	go func() {
		done <- intoSharedLink("python3 - <<'PY'\nprint(\"could not find ) in the text\")\nx = s.find )\nPY\nfind ~/PC -name '*.pdf'")
	}()
	select {
	case got := <-done:
		if !strings.Contains(got, "find ~/PC/ -name") {
			t.Errorf("the real find was not made to start inside the link: %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("intoSharedLink did not end")
	}
}

// Found live (2026-10-25): a "prepare, do not file" task folded a service line into a goods line of a summary; the
// totals tied and no commit came, so nothing worked the figures out again. At a report, the screen the work leaves
// is checked like a commit's - only its own window's figures.
func TestTheFiguresAWorkLeavesOnScreenAreWorkedOutAgainAtTheReport(t *testing.T) {
	listing := "Screen 1280x720. Active window: \"Summary | Portal - Chromium\"\n" +
		"== Summary | Portal - Chromium ==\n" + `row "7312 | 901 | 130,347.46" (e11)` + "\n" + `row "9954 | 2 | 1,000.00" (e12)` + "\n" +
		"== Thunar frame \"Files\" ==\n" + `status "6 files: 343.4 KiB" (e90)`
	chat := &scriptedChat{replies: []string{`{"v1": "7312", "v2": "775", "v3": "103847.46", "v4": "9954", "v5": "2", "v6": "1000"}`}}
	st := &operatorState{Hosted: true, Request: "prepare the return, do not file", Listings: []string{listing}}
	issues := (&OperatorSkill{}).verifyCommit(weighRC(chat), st, "")
	if len(issues) != 2 || !strings.Contains(issues[1].What, "130,347.46") || !strings.Contains(issues[1].Why, "before reporting") {
		t.Fatalf("issues = %+v", issues)
	}
	if sent := chat.calls[0][1].Content; strings.Contains(sent, "KiB") || !strings.Contains(sent, "AS IT REPORTS IT DONE") {
		t.Errorf("sent = %s", sent)
	}
	if typedOnScreen(&operatorState{Worked: []string{"gui observe", "office read x.pdf"}}) ||
		!typedOnScreen(&operatorState{Worked: []string{`gui click e1 && gui type e5 "26500"`}}) {
		t.Error("typedOnScreen")
	}
}

// Found live (2026-10-25): a return keyed over several pages of one site was checked at the report only on its last
// page. Every page typed into is worked out (its newest look), and the screen the work leaves when it is of the same
// site - never another program's window.
func TestAtAReportEveryPageTypedIntoIsWorkedOutAgain(t *testing.T) {
	st := &operatorState{Hosted: true, Request: "prepare the return, do not file"}
	sales := "Screen 1280x720. Active window: \"Sales | Portal - Chromium\"\n== Sales | Portal - Chromium ==\n" +
		`row "29 | 12% | 34,750 | 2,085" (e21)` + "\n"
	st.noteWindow("gui observe", shellResult{Output: sales})
	st.noteWindow(`gui type e21 "34750" && gui click e22`, shellResult{Output: "Typed.\nactive window is now \"Rates | Portal - Chromium\""})
	if len(st.TypedWindows) != 1 || st.TypedWindows[0] != "Sales | Portal - Chromium" || st.ActiveWindow != "Rates | Portal - Chromium" {
		t.Fatalf("after typing then moving on: %+v / %q", st.TypedWindows, st.ActiveWindow)
	}
	st.noteWindow(`gui select e30 "5%"`, shellResult{Output: "Chose."})
	st.noteWindow(`gui type e21 "1"`, shellResult{Output: "window \"Sales | Portal - Chromium\" opened"})
	if want := []string{"Sales | Portal - Chromium", "Rates | Portal - Chromium"}; strings.Join(st.TypedWindows, ";") != strings.Join(want, ";") {
		t.Fatalf("typed windows = %v", st.TypedWindows)
	}
	st.noteWindow(`gui click e5 && gui type e6 "2"`, shellResult{Output: "active window is now \"Notes | Portal - Chromium\""})
	if want := []string{"Rates | Portal - Chromium", "Sales | Portal - Chromium", "Notes | Portal - Chromium"}; strings.Join(st.TypedWindows, ";") != strings.Join(want, ";") {
		t.Fatalf("typing after a click that moved on: %v", st.TypedWindows)
	}
	st.noteWindow(`gui type e9 "x"`, shellResult{Output: "error", ExitCode: 1})
	if len(st.TypedWindows) != 3 {
		t.Fatalf("a failed typing was noted: %v", st.TypedWindows)
	}
	st.TypedWindows = []string{"Sales | Portal - Chromium"}

	chatWin := "Screen 1280x720. Active window: \"Chat - Client - Chromium\"\n== Chat - Client - Chromium ==\n" +
		`text "send 34,750 and 2,085 please" (e40)` + "\n"
	summary := "Screen 1280x720. Active window: \"Summary | Portal - Chromium\"\n== Summary | Portal - Chromium ==\n" +
		`row "7 | 34,750 | 2,085 | 2,085" (e50)` + "\n"
	st.Listings = []string{sales, chatWin, summary}
	st.Worked = []string{"gui observe"} // looked last: nothing to look at again
	chat := &scriptedChat{replies: []string{
		`{"v1": "7", "v2": "34750", "v3": "1508.75", "v4": "1508.75"}`,
		`{"v1": "29", "v2": "24000", "v3": "1440"}`,
	}}
	issues := (&OperatorSkill{}).verifyReport(weighRC(chat), st)
	if len(chat.calls) != 2 {
		t.Fatalf("screens checked = %d, want the summary and the typed page", len(chat.calls))
	}
	first, second := chat.calls[0][1].Content, chat.calls[1][1].Content
	if !strings.Contains(first, "AS IT REPORTS IT DONE") || !strings.Contains(first, "Summary | Portal") ||
		!strings.Contains(second, "A SCREEN THE WORK TYPED INTO") || !strings.Contains(second, "Sales | Portal") {
		t.Fatalf("sent:\n%s\n---\n%s", first, second)
	}
	for _, c := range chat.calls {
		if strings.Contains(c[1].Content, "send 34,750") {
			t.Fatal("another program's window was worked out")
		}
	}
	if len(issues) != 4 {
		t.Fatalf("issues = %+v", issues)
	}
	if (&OperatorSkill{}).verifyReport(weighRC(&scriptedChat{}), &operatorState{Hosted: true, Listings: []string{summary}}) != nil {
		t.Error("checked a screen with nothing typed")
	}
}

func TestSiteOfAWindowTitle(t *testing.T) {
	for title, want := range map[string]string{
		"Summary | Portal - Chromium":                 "Portal",
		"Invoice - 451 | Portal - Chromium":           "Portal",
		"B2C Supplies | Goods and Services Tax - Ch…": "Goods and Services Tax",
		"book.xlsx - LibreOffice Calc":                "book.xlsx",
		"Chat - Client - Chromium":                    "Chat - Client",
		"ab":                                          "",
	} {
		if got := siteOf(title); got != want {
			t.Errorf("siteOf(%q) = %q, want %q", title, got, want)
		}
	}
}

// Found live (2026-10-25): the check at a report put eight meaningless points to the person - figures of an action's
// "What changed" line before the listing, a filing's clock time, and a form's section numbers in its labels.
func TestOnlyWhatAnElementSaysIsWorkedOut(t *testing.T) {
	st := &operatorState{}
	st.noteListing("gui click e5 && gui observe", "Clicked.\nWhat changed: page is now http://x:8703/r?period=092026; 62 element(s) appeared\n"+
		"Screen 1280x720. Active window: \"Return | Portal - Chromium\"\n== Return | Portal - Chromium ==\n"+
		`e1 heading "3.1 Details of Outward Supplies"`+"\n"+
		`e2 text "Filed on 25/10/2026 02:40. ARN AA2909262645093B"`+"\n"+
		`e3 row "(A)(3) ITC available"`+"\n"+
		`e4 textbox "(A)(3) ITC on inward supplies (other than 1 & 2)" value="1,440.00" [row 3]`+"\n"+
		`e5 textbox "Notes" value="first line" (61 chars, 2 lines - gui read e5 shows all)`+"\n"+
		`e6 row "7312 | 901 | 130,347.46"`+"\n"+
		`e7 link "5 - B2C (Large) Invoices"`+"\n"+`e8 button "Add 2 rows"`+"\n"+`e9 text "3"`)
	if len(st.Listings) != 1 || strings.Contains(st.Listings[0], "What changed") {
		t.Fatalf("listing kept: %q", st.Listings)
	}
	masked, figures := maskedFigures(st.Listings[0])
	want := map[string]string{"v1": "1,440.00", "v2": "7312", "v3": "901", "v4": "130,347.46", "v5": "3"}
	if fmt.Sprint(figures) != fmt.Sprint(want) {
		t.Fatalf("figures = %v\nmasked:\n%s", figures, masked)
	}
	if !strings.Contains(masked, `e4 textbox "(A)(3) ITC on inward supplies (other than 1 & 2)" value="[v1]" [row 3]`) {
		t.Errorf("the field's label or states were blanked:\n%s", masked)
	}
}

// The same blanked screen against the same sources is worked out once a goal: a later report round sets that working
// against the screen's figures as they are then - a figure put right since is no longer a point.
func TestTheSameScreenAndSourcesAreWorkedOutOnce(t *testing.T) {
	screen := func(taxable string) string {
		return "Screen 1280x720. Active window: \"Sales | Portal - Chromium\"\n== Sales | Portal - Chromium ==\n" +
			`e21 row "29 | ` + taxable + ` | 2,085"`
	}
	chat := &scriptedChat{replies: []string{`{"v1": "29", "v2": "24000", "v3": "2085"}`}}
	st := &operatorState{Hosted: true, Request: "enter the month", TypedWindows: []string{"Sales | Portal - Chromium"},
		TypedFigures: map[string]bool{"Sales | Portal - Chromium": true},
		Listings: []string{screen("34,750")}, Worked: []string{"gui observe"}}
	if issues := (&OperatorSkill{}).verifyReport(weighRC(chat), st); len(issues) != 1 {
		t.Fatalf("first check: %+v", issues)
	}
	st.Listings = []string{screen("24,000")}
	if issues := (&OperatorSkill{}).verifyReport(weighRC(chat), st); len(issues) != 0 || len(chat.calls) != 1 {
		t.Fatalf("second check: %+v, %d model calls", issues, len(chat.calls))
	}
}

// Found live (2026-10-25): a summary table's header row holds no figure and was dropped; the verifier read the blanks
// of a row with no names for its columns and set a value against a count. The header goes in front of its rows.
func TestATablesHeaderIsShownWithItsBlankedRows(t *testing.T) {
	listing := "Screen 1280x720. Active window: \"Summary | Portal - Chromium\"\n== Summary | Portal - Chromium ==\n" +
		`e1 row "Description | No. of records | Taxable value | Tax"` + "\n" +
		`e2 row "Sales | 4 | 219,722.46 | 19,775.02"` + "\n" + `e3 row "Returns | 1 | 500.00 | 45.00"` + "\n" +
		`e4 text "Prepared by the clerk"` + "\n" + `e5 row "Notes | none | kept"` + "\n" + `e6 text "Total 220,222.46"`
	masked, figures := maskedFigures(listing)
	want := `e1 row "Description | No. of records | Taxable value | Tax"` + "\n" +
		`e2 row "Sales | [v1] | [v2] | [v3]"` + "\n" + `e3 row "Returns | [v4] | [v5] | [v6]"` + "\n" + `e6 text "Total [v7]"`
	if masked != want || len(figures) != 7 {
		t.Fatalf("masked:\n%s\nfigures %v", masked, figures)
	}
}

// Found live (2026-10-25): a summary's rows were looked at only while empty; typed wrong, they were never seen again
// and nothing checked them. A window typed in after its last look is a point at the report - if it can still be
// looked at (open, or a page of a site open now), never a dialog that closed.
func TestAWindowTypedInAfterItsLastLookIsLookedAtAgain(t *testing.T) {
	now := "Screen 1280x720. Active window: \"Summary | Portal - Chromium\"\nOpen windows: \"Summary | Portal - Chromium\", \"Files\"\n"
	st := &operatorState{
		TypedWindows: []string{"Save As", "Rows | Portal - Chromium"},
		TypedAt:      map[string]int{"Save As": 4, "Rows | Portal - Chromium": 5},
		ListedAt:     map[string]int{"Rows | Portal - Chromium": 3, "Summary | Portal - Chromium": 7},
		Listings:     []string{now},
	}
	issues := st.unseenTyping()
	if len(issues) != 1 || !strings.Contains(issues[0].What, "Rows | Portal") || issues[0].Settle != "recheck" {
		t.Fatalf("issues = %+v", issues)
	}
	// Told once a goal (2026-10-26: a login typed into on the way was raised on every review batch).
	if issues := st.unseenTyping(); len(issues) != 0 {
		t.Fatalf("raised twice: %+v", issues)
	}
	st.UnseenTold = nil
	st.ListedAt["Rows | Portal - Chromium"] = 6
	if issues := st.unseenTyping(); len(issues) != 0 {
		t.Fatalf("looked at after the typing, still a point: %+v", issues)
	}
	// Typing and a look in one command: seen.
	st = &operatorState{Worked: []string{"x"}}
	st.noteListing("gui observe", now)
	st.noteWindow(`gui type e5 "1" && gui observe`, shellResult{Output: now})
	if issues := st.unseenTyping(); len(issues) != 0 {
		t.Fatalf("typing and a look in one command: %+v", issues)
	}
}

// Found live (2026-10-25): a photo's two lines, each with its own code and rate, were written as one row whose code
// was "MIXED" - a word no source holds. A one-word cell value nothing the goal was shown holds is a point at the
// report, unless the report names it; a later write of the cell clears it. The work's own script is no source.
func TestAOneWordValueNoSourceHoldsIsAPointAtTheReport(t *testing.T) {
	st := &operatorState{Request: "enter the month's bills"}
	st.addUnit("document", "bill_457.jpg", "Bill 457: Rice 1006 qty 50 2,750; Pickle 2001 qty 40 6,000")
	st.addLines("row", "BOOK.xlsx", "[SALES] B20=Rice C20=OTH D20=455")
	st.addLines("computed", "python3 -", "('457', 'MIXED', 8750)")
	write := func(cell, value string) {
		st.noteWritten("office update 'BOOK.xlsx' - <<'EOF'\n{\"edits\": [{\"sheet\": \"SALES\", \"cell\": \"" + cell +
			"\", \"value\": \"" + value + "\"}]}\nEOF")
	}
	write("E31", "MIXED")
	write("B31", "Rice")
	write("G31", "OTH")
	if len(st.InventedCodes) != 1 || !strings.Contains(st.InventedCodes[0], `"MIXED" (SALES!E31)`) {
		t.Fatalf("invented = %v", st.InventedCodes)
	}
	if issues := st.inventedCodeIssues("Entered the bills."); len(issues) != 1 || issues[0].Settle != "recheck" {
		t.Fatalf("issues = %+v", issues)
	}
	if issues := st.inventedCodeIssues("E31 holds MIXED because ..."); len(issues) != 0 {
		t.Fatalf("named in the report, still a point: %+v", issues)
	}
	write("E31", "1006")
	if len(st.InventedCodes) != 0 {
		t.Fatalf("the cell written again still counted: %v", st.InventedCodes)
	}
}

// Found live (2026-10-25): a photo bill the work could not read was "accounted for" by a sentence saying so, and the
// month went out a bill short. Not being able to read a document is no reason for it to be out of the work.
func TestCouldNotReadIsNoReasonToLeaveADocumentOut(t *testing.T) {
	base, stem := "img_0932.jpg", "img_0932"
	for _, report := range []string{
		"img_0932.jpg is part of the thread, but its contents were not successfully extracted, so it did not change the figures.",
		"I could not read img_0932.jpg, so it is not in the sales block.",
	} {
		if saysWhyLeftOut(strings.ToLower(report), base, stem) {
			t.Errorf("settled by: %s", report)
		}
	}
	if !saysWhyLeftOut("img_0932.jpg is not part of the work: it is another client's bill.", base, stem) {
		t.Error("a reason was not taken")
	}
}

// Found live (2026-10-25): six bills read with `for f in *.docx; do office read "$f"; done` were "never read" to the
// record. Each read in a loop or through a pattern is credited by the header its output opens with.
func TestFilesReadThroughALoopAreRead(t *testing.T) {
	st := &operatorState{}
	out := "1188 Coastal Infra Projects.docx: Word document, 1 page\nBill No. : 1188\nTotal 76,700.00\n" +
		"1189 Kasaragod Arecanut Processors.docx: Word document, 1 page\nBill No. : 1189\nTotal 1,74,640.00\n"
	st.noteEvidence(`for f in /home/headless/Downloads/*.docx; do office read "$f"; done`, out)
	var docs []string
	for _, u := range st.Units {
		if u.Kind == "document" {
			docs = append(docs, u.Source)
		}
	}
	if strings.Join(docs, "|") != "1188 Coastal Infra Projects.docx|1189 Kasaragod Arecanut Processors.docx" {
		t.Fatalf("documents = %v", docs)
	}
	if !strings.Contains(st.Units[1].Text, "1,74,640.00") || strings.Contains(st.Units[0].Text, "1189") {
		t.Errorf("each document holds its own part: %+v", st.Units[:2])
	}
}

// Found live (2026-10-25): a find that listed six bills reached the review as its first line, and the review held the
// folder had only that one. A short output goes whole; a long one says how much more there was.
func TestTheLedgerKeepsAShortOutputWhole(t *testing.T) {
	short := shellResult{Output: "./7. OCTOBER/1188 a.docx\n./7. OCTOBER/1189 b.docx\n./7. OCTOBER/1190 c.docx"}
	if got := ledgerOutput(short); !strings.Contains(got, "1190 c.docx") || !strings.Contains(got, " | ") {
		t.Errorf("short output: %s", got)
	}
	long := shellResult{Output: strings.Repeat("a line of a long listing\n", 40)}
	if got := ledgerOutput(long); !strings.Contains(got, "(+39 more lines)") {
		t.Errorf("long output: %s", got)
	}
}

// Found live (2026-10-25): four bills whose amounts were typed by chains that failed at a later step were "read; none
// of its figures went into what was entered". What a failed chain's output says it typed is credited.
func TestWhatAFailedChainTypedIsCredited(t *testing.T) {
	st := &operatorState{}
	st.noteTypedShown(`gui type e5 "7350.50" && gui type e6 "661.55" && gui click --see "Kerala in the list"`,
		"Typed \"7350.50\" into textbox \"Amount 1\" (e5).\nTyped \"661.55\" into textbox \"Amount 2\" (e6).\ngui: nothing like that on the screen")
	if strings.Join(st.Written, " ") != "7350.50 661.55" {
		t.Fatalf("written = %v", st.Written)
	}
	st.noteTypedShown(`gui type e7 "8674.00"`, "gui: e7 is not on the screen")
	if contains(st.Written, "8674.00") {
		t.Error("a value the output does not say was typed was credited")
	}
}

// Found live (2026-10-25): the figure check at a report took a form's default ("No." 1) for the work - from a look
// before the typing, and from a fresh form the application opened after the save. Neither is the work.
func TestAReportsCheckNeverWorksOutAFormsDefaults(t *testing.T) {
	form := "Screen 1280x720. Active window: \"New voucher | Books - Chromium\"\n== New voucher | Books - Chromium ==\n" +
		`e1 textbox "No." value="1"` + "\n" + `e2 textbox "Amount 1" value="7350.50"` + "\n" + `e3 row "Total | 8,674.00"`
	st := &operatorState{Worked: []string{`gui type e2 "7350.50"`}}
	masked, figures := maskedFiguresOf(windowPart(form, "New voucher | Books - Chromium"), st.typedByWork)
	if strings.Contains(masked, `"No." value="[v`) || len(figures) != 2 {
		t.Fatalf("masked:\n%s\n%v", masked, figures)
	}
	st.TypedAt = map[string]int{"Bills | Books - Chromium": 5}
	st.ListedAt = map[string]int{"Bills | Books - Chromium": 3}
	if !st.typedSinceLook("Bills | Books - Chromium") || st.typedSinceLook("Summary | Books - Chromium") {
		t.Error("typedSinceLook")
	}
}

// Found live (2026-10-26): a bill's row in a register was written over by the next bill's; the first bill was read,
// counted as written, and never in the file - no point was raised.
func TestAValueWrittenOverIsNoLongerWritten(t *testing.T) {
	st := &operatorState{Hosted: true}
	st.noteWritten(`office update /w/calc.xlsx - <<'EOF'
{"edits": [{"sheet": "SALES", "cell": "D25", "value": 453}, {"sheet": "SALES", "cell": "F25", "value": 18000}]}
EOF`)
	if !contains(st.Written, "453") || !contains(st.Written, "18000") {
		t.Fatalf("written = %v", st.Written)
	}
	st.noteReplaced("Updated /w/calc.xlsx: 3 cell(s) (SALES!B25, SALES!D25, SALES!F25); replaced 2 filled cell(s): SALES!D25 453 -> 452, SALES!F25 18000 -> 24000. Formulas recalculate when the file is opened.")
	if contains(st.Written, "453") || contains(st.Written, "18000") {
		t.Errorf("written over, still written: %v", st.Written)
	}
}

// Found live (2026-10-27): a chat window the work typed a message into had another client's "1,620" in a preview;
// the verifier set it against this task's sources and the approval carried the point.
func TestAWindowTypedOnlyProseIntoCarriesNoFigureOfTheWork(t *testing.T) {
	st := &operatorState{Hosted: true, ActiveWindow: "Chat - Alpha - Chromium"}
	st.noteWindow(`gui type e5 "Hello, the September OTP has been sent. Please reply with it."`, shellResult{Output: `Typed "Hello" into textbox "Type a message" (e5)`})
	if st.typedFigureIn("Chat - Alpha - Chromium") {
		t.Fatalf("prose counted as a figure")
	}
	st.ActiveWindow = "Voucher - Chromium"
	st.noteWindow(`gui type e9 "26,500.00"`, shellResult{Output: `Typed "26,500.00" into textbox "Amount" (e9)`})
	if !st.typedFigureIn("Voucher - Chromium") {
		t.Fatalf("a typed amount not counted")
	}
}
