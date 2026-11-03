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

// A row from another sender counts only when it names a number this task's work holds - by that mechanical fact, never
// by words in it that sound like a revision (review T-18).
func TestAMessageFromAnotherSenderIsNotRaisedUnlessItNamesThisTasksNumber(t *testing.T) {
	plain := &operatorState{Hosted: true, Listings: []string{`e20 row "Office - Note on 4417 | the file for 4417 is attached | 2026-12-04"`,
		`e21 row "Office - Revised plan, updated and corrected | see attached | 2026-12-04"`}}
	plain.Worked = append(plain.Worked, `office read '/w/B-4417.pdf'`)
	got := plain.unopenedMessageIssues("Done.")
	if len(got) != 1 || !strings.Contains(got[0].What, "Note on 4417") {
		t.Fatalf("want only the row naming the task's number, got %+v", got)
	}
	st := &operatorState{Hosted: true, Listings: []string{inboxListing}}
	// Nothing opened yet: no sender is this task's; only a row naming a document this task read counts.
	if got := st.unopenedMessageIssues("Done."); len(got) != 0 {
		t.Errorf("rows raised with nothing opened and nothing read: %+v", got)
	}
	st.Worked = append(st.Worked, `office read '/w/ABC-0201.pdf'`)
	got = st.unopenedMessageIssues("Done.")
	if len(got) != 1 || !strings.Contains(got[0].What, "Revised invoice 0201") {
		t.Fatalf("a revision of a document read is not a point: %+v", got)
	}
	// The subject stands as the page's heading: opened without a click (a keyboard, a link by name) - and now the
	// sender's other message is the one never opened.
	st.noteSeen(`heading "Revised invoice 0201"`)
	got = st.unopenedMessageIssues("Done.")
	if len(got) != 1 || !strings.Contains(got[0].What, "November invoices") {
		t.Errorf("after the page was seen: %+v", got)
	}
}
