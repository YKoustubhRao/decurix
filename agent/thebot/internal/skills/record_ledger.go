package skills

import (
	"regexp"
	"strings"
	"time"
)

// The record ledger the host keeps (gui record / gui records): each record the work declared from its document, what
// it meant to enter, and what the page showed after the save. Read before a review so the reviewer judges facts about
// records, not the report's prose; and turned into points in code where the ledger itself says the work is not whole.

// maxReviewLedgerChars bounds the ledger shown to the reviewer; maxReviewListings and maxReviewListingChars bound the
// newest window listings shown with it.
const (
	maxReviewLedgerChars  = 6000
	maxReviewListings     = 4
	maxReviewListingChars = 3000
)

// recordLedgerCommand prints the ledger.
const recordLedgerCommand = "gui records"

// readRecordLedger runs gui records and keeps what it printed, when the goal typed on a screen of a hosted desktop.
func (o *OperatorSkill) readRecordLedger(rc RunContext, st *operatorState) {
	if !st.Hosted || !typedOnScreen(st) {
		return
	}
	res := runShell(rc.Ctx, rc.Dir(), recordLedgerCommand, 30*time.Second, st.guiEnv()...)
	if res.ExitCode != 0 {
		return
	}
	st.RecordLedger = strings.TrimSpace(res.Output)
}

// Lines of the ledger that say a record is not whole: "- KEY (from ...): declared, not saved ..." and
// "- KEY (from ...): saved; not read back: Field, Field".
var (
	ledgerRecordRe   = regexp.MustCompile(`(?m)^- (.+?) \(from [^)]*\): (.*)$`)
	ledgerNotSavedRe = regexp.MustCompile(`^declared, not saved`)
	ledgerAbsentRe   = regexp.MustCompile(`^saved; not read back: (.+)$`)
)

// recordLedgerIssues are the review points the ledger decides on its own: a record declared and never saved, and a
// record saved whose declared values the page did not all show.
func recordLedgerIssues(ledger string) []reviewIssue {
	var out []reviewIssue
	for _, m := range ledgerRecordRe.FindAllStringSubmatch(ledger, -1) {
		key, status := strings.TrimSpace(m[1]), strings.TrimSpace(m[2])
		if ledgerNotSavedRe.MatchString(status) {
			out = append(out, reviewIssue{
				What:   "record " + key + " was declared from its document but never saved",
				Why:    "the ledger shows no save for it" + strings.TrimPrefix(status, "declared, not saved"),
				Settle: "save it, or say in the report why it is left out",
				Work:   true,
			})
			continue
		}
		if a := ledgerAbsentRe.FindStringSubmatch(status); a != nil {
			out = append(out, reviewIssue{
				What:   "record " + key + " was saved, but the page did not show these declared values: " + a[1],
				Why:    "a value the application shows otherwise than the document gives it is not entered as the document says",
				Settle: "open the saved record and read those fields; correct the record, or say what the application shows and why it is right",
				Work:   true,
			})
		}
	}
	return out
}
