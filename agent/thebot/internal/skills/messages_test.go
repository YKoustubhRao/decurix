package skills

import (
	"strings"
	"testing"
)

const inboxListing = `Screen 1280x720. Active window: "Mail - Chromium"
== Chromium tab "Mail" http://host/mail ==
e10 row "Careers Weekly - 5 new openings near you | 5 new openings - Accounts executive | 2026-12-05 08:26"
e11 row "Asha Rao - November invoices | November invoices - Dear Sir, please find the bills | 2026-12-03 10:02"
e12 row "Asha Rao - Revised invoice 0201 | Revised invoice 0201 - Please take this revised invoice for 0201 | 2026-12-04 09:11"
e13 row "Ravi (Office) - Leave on Monday | Leave on Monday - Sir, I will be on leave | 2026-12-04 19:42"`

// Found live (2026-10-26, twice): the client's revised bill stood in the inbox two rows under the mail the worker
// opened, and was never opened; the report never named it.
func TestAMessageFromAnOpenedSenderThatWasNeverOpenedIsThePersonsWorkNotTheirQuestion(t *testing.T) {
	st := &operatorState{Hosted: true, Listings: []string{inboxListing}}
	st.Worked = append(st.Worked, "gui click e11 && gui observe", `office read '/w/ABC-0201.pdf'`)
	issues := st.unopenedMessageIssues("Done. Entered the five November bills.")
	if len(issues) != 1 || !strings.Contains(issues[0].What, "Asha Rao - Revised invoice 0201") || !issues[0].Work {
		t.Fatalf("issues = %+v", issues)
	}
	if got := st.unopenedMessageIssues("Done. Entered five bills; the mail \"Revised invoice 0201\" is a copy of 0201 already entered, left out."); len(got) != 0 {
		t.Errorf("a message the report names is still a point: %+v", got)
	}
	st.Worked = append(st.Worked, "gui click e12 && gui observe")
	if got := st.unopenedMessageIssues("Done."); len(got) != 0 {
		t.Errorf("an opened message is still a point: %+v", got)
	}
}

// A row from another sender counts only when its subject names a record this task declared, as a whole token - never
// by words that sound like a revision (T-18), and never by a number in its preview or date (round 2, R-2).
func TestAMessageFromAnotherSenderIsNotRaisedUnlessItNamesThisTasksRecord(t *testing.T) {
	plain := &operatorState{Hosted: true, RecordLedger: "- B-4417 (from /w/B-4417.pdf): saved and read back",
		Listings: []string{`e20 row "Office - Note on 4417 | the file is attached | 2026-12-04"`,
			`e21 row "Office - Revised plan, updated and corrected | see 4417 attached | 2026-12-04"`,
			`e22 row "Office - Week 2026 | 44170 in stock | 2026-12-04"`}}
	plain.Worked = append(plain.Worked, `office read '/w/B-4417.pdf'`, `ls /w/2026`)
	got := plain.unopenedMessageIssues("Done.")
	if len(got) != 1 || !strings.Contains(got[0].What, "Note on 4417") {
		t.Fatalf("want only the row whose subject names the declared record, got %+v", got)
	}
	st := &operatorState{Hosted: true, Listings: []string{inboxListing}}
	// Nothing opened and nothing declared: no sender is this task's, and no row names a record of it.
	st.Worked = append(st.Worked, `office read '/w/ABC-0201.pdf'`)
	if got := st.unopenedMessageIssues("Done."); len(got) != 0 {
		t.Errorf("rows raised with nothing opened and nothing declared: %+v", got)
	}
	st.RecordLedger = "- ABC-0201 (from /w/ABC-0201.pdf): saved"
	got = st.unopenedMessageIssues("Done.")
	if len(got) != 1 || !strings.Contains(got[0].What, "Revised invoice 0201") {
		t.Fatalf("a row naming a declared record is not a point: %+v", got)
	}
	// The subject stands as the page's heading: opened without a click (a keyboard, a link by name) - and now the
	// sender's other message is the one never opened.
	st.noteSeen(`heading "Revised invoice 0201"`)
	got = st.unopenedMessageIssues("Done.")
	if len(got) != 1 || !strings.Contains(got[0].What, "November invoices") {
		t.Errorf("after the page was seen: %+v", got)
	}
}

// A key whose parts include a year names its record only by its own tokens or its running number: a subject that
// shares only the year raises nothing (round 3, N-W3-5).
func TestAYearInADeclaredKeyNamesNoRecordAlone(t *testing.T) {
	ledger := "- INV/2026/0201 (from /w/inv-0201.pdf): saved"
	for subject, want := range map[string]bool{
		"Plans for 2026":            false,
		"Revised 0201":              true,
		"INV/2026/0201 corrected":   true,
		"Order 02011 for next week": false,
	} {
		st := &operatorState{Hosted: true, RecordLedger: ledger,
			Listings: []string{`e30 row "Office - ` + subject + ` | see attached | 2026-12-04"`}}
		if got := len(st.unopenedMessageIssues("Done.")) == 1; got != want {
			t.Errorf("subject %q: raised = %v, want %v", subject, got, want)
		}
	}
}
