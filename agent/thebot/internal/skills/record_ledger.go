package skills

import (
	"fmt"
	"path"
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
// The source may itself hold brackets ("Name (Firm) 0195.pdf"): it runs to the last "): " of the line, and
// a "; scope ..." after it is not part of it. Found 2026-10-28: "[^)]*" stopped at the first ")" and the record's
// points were silently skipped.
var (
	ledgerRecordRe   = regexp.MustCompile(`(?m)^- (.+?) \(from (.*?)(?:; scope [^\n]*?)?\): (.*)$`)
	ledgerNotSavedRe = regexp.MustCompile(`^declared, not saved`)
	ledgerAbsentRe   = regexp.MustCompile(`^saved; not read back: (.+)$`)
)

// recordLedgerIssues are the review points the ledger decides on its own: a record declared and never saved, and a
// record saved whose declared values the page did not all show.
func recordLedgerIssues(ledger string) []reviewIssue {
	var out []reviewIssue
	for _, m := range ledgerRecordRe.FindAllStringSubmatch(ledger, -1) {
		key, status := strings.TrimSpace(m[1]), strings.TrimSpace(m[3])
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

// ledgerSources are the base names (lower case) of the files the ledger's records were declared from; empty when no
// record came from a file (one from the screen or the request names no file).
func ledgerSources(ledger string) map[string]bool {
	out := map[string]bool{}
	for _, m := range ledgerRecordRe.FindAllStringSubmatch(ledger, -1) {
		src := strings.TrimSpace(m[2])
		if src == "" || !strings.Contains(src, "/") {
			continue
		}
		out[strings.ToLower(path.Base(src))] = true
	}
	return out
}

// ledgerFieldRe is one declared field of a record in the ledger: "    Name: value [source; read back: seen]".
var ledgerFieldRe = regexp.MustCompile(`^\s+(.+?): (.*?)(?: \[[^\]]*\])?\s*$`)

// rowFieldRe is a field of a form's row: a name and its row number ("Particulars 2", "Amount 2").
var rowFieldRe = regexp.MustCompile(`^(.*\S)\s+(\d{1,2})$`)

// rateRe is a rate written in a line's label ("Tax@2.5%", "Discount 10 %").
var rateRe = regexp.MustCompile(`(\d{1,2}(?:\.\d{1,3})?)\s*%`)

// ledgerAmount is a figure as a form holds it ("12,500.00", "312.5"); ok false for anything else.
func ledgerAmount(v string) (float64, bool) {
	v = strings.ReplaceAll(strings.TrimSpace(v), ",", "")
	if v == "" || strings.ContainsAny(v, "%/") {
		return 0, false
	}
	var f float64
	if _, err := fmt.Sscan(v, &f); err != nil {
		return 0, false
	}
	return f, true
}

// rateLineFacts are arithmetic facts about a declared record's rate lines, for the reviewer to weigh - no verdict: a
// row whose label names a rate ("@2.5%", "10 %") and whose amount is not the largest of the record, when that rate of
// none of the other amounts (nor of their sum) is its amount. The share it does imply is stated; whether a figure was
// misread, or the line is charged on something the record does not hold, is the reviewer's to judge with the document
// (review T-18: a code verdict on a "rate line" sent the worker back by a word in a label).
func rateLineFacts(ledger string) []string {
	var out []string
	lines := strings.Split(ledger, "\n")
	for i := 0; i < len(lines); i++ {
		m := ledgerRecordRe.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		key := strings.TrimSpace(m[1])
		rows := map[string]map[string]string{}
		for j := i + 1; j < len(lines) && !strings.HasPrefix(lines[j], "- "); j++ {
			f := ledgerFieldRe.FindStringSubmatch(lines[j])
			if f == nil {
				continue
			}
			if r := rowFieldRe.FindStringSubmatch(strings.TrimSpace(f[1])); r != nil {
				if rows[r[2]] == nil {
					rows[r[2]] = map[string]string{}
				}
				rows[r[2]][r[1]] = strings.TrimSpace(f[2])
			}
		}
		type line struct {
			label  string
			rate   float64
			amount float64
		}
		var all []line
		largest := 0.0
		for _, fields := range rows {
			var l line
			hasAmount := false
			for _, v := range fields {
				if a, ok := ledgerAmount(v); ok {
					l.amount, hasAmount = a, true
				} else if r := rateRe.FindStringSubmatch(v); r != nil {
					fmt.Sscan(r[1], &l.rate)
					l.label = v
				}
			}
			if !hasAmount {
				continue
			}
			all = append(all, l)
			if l.amount > largest {
				largest = l.amount
			}
		}
		for _, l := range all {
			if l.rate <= 0 || l.amount <= 0 || l.amount >= largest {
				continue
			}
			fits, base := false, 0.0
			for _, o := range all {
				if o.amount > l.amount {
					base += o.amount
					if within(o.amount*l.rate/100, l.amount) {
						fits = true
					}
				}
			}
			if fits || within(base*l.rate/100, l.amount) {
				continue
			}
			out = append(out, fmt.Sprintf("record %s: the line %q holds %.2f, which is %.4g%% of %.2f; no amount in "+
				"the record, nor their sum, is %.2f", key, l.label, l.amount, l.rate, l.amount*100/l.rate, l.amount*100/l.rate))
		}
	}
	return out
}

// within reports whether two amounts agree to the paisa a rounding can move (1 unit).
func within(a, b float64) bool {
	d := a - b
	return d > -1.01 && d < 1.01
}
