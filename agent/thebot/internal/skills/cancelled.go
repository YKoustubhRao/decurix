package skills

import (
	"regexp"
	"strings"
)

// A record saved that a message the task read calls cancelled. Found live (2026-10-27): the client's chat, opened and
// listed by the worker, read "3309 cancel aagide sir, customer sprayer return maadidru"; the plan question that once
// brought the person's word on it was answered from a kept lesson, and voucher 3309 was saved with the rest. The
// message's words and the saved record's number are both on record: decided in code.

// cancelWordRe is a word that says a document no longer stands.
var cancelWordRe = regexp.MustCompile(`(?i)\b(cancel\w*|void\w*|withdraw\w*|return(?:ed|s)?|reversed|annul\w*|scrap\w*)\b`)

// savedLedgerKeyRe is a record the ledger shows saved: "- KEY (from ...): saved ...".
var savedLedgerKeyRe = regexp.MustCompile(`(?m)^- (\S+) \(from [^)]*\): saved`)

// maxCancelledIssues bounds the points raised.
const maxCancelledIssues = 3

// cancelledSavedIssues are Work points for records saved whose number a line the task read (a message, a page) calls
// cancelled, returned or void - unless the report names the number beside such a word.
func (st *operatorState) cancelledSavedIssues(report string) []reviewIssue {
	if st.RecordLedger == "" {
		return nil
	}
	var keys []string
	for _, m := range savedLedgerKeyRe.FindAllStringSubmatch(st.RecordLedger, -1) {
		keys = append(keys, m[1])
	}
	if len(keys) == 0 {
		return nil
	}
	said := strings.ToLower(report)
	var out []reviewIssue
	seen := map[string]bool{}
	for _, line := range strings.Split(st.Seen+"\n"+strings.Join(st.Listings, "\n"), "\n") {
		if !cancelWordRe.MatchString(line) {
			continue
		}
		for _, n := range digitRunRe.FindAllString(line, -1) {
			if seen[n] {
				continue
			}
			for _, k := range keys {
				if !keyHoldsNumber(k, n) {
					continue
				}
				seen[n] = true
				if strings.Contains(said, n) && cancelWordRe.MatchString(said) {
					break // the report says what it did about it
				}
				out = append(out, reviewIssue{
					What: "record " + k + " was saved, but a line this task read calls " + n + " off: \"" +
						truncateText(strings.TrimSpace(line), 200) + "\"",
					Why: "a document the client cancelled or took back is not entered",
					Settle: "read that message whole; if it cancels this document, remove the saved entry the " +
						"application's way and say so in the report; if it does not, say in the report why the entry stands",
					Work: true,
				})
				break
			}
			if len(out) == maxCancelledIssues {
				return out
			}
		}
	}
	return out
}

// keyHoldsNumber reports whether a record's key holds the number as a whole run of digits ("ABC3309", "3309").
func keyHoldsNumber(key, n string) bool {
	for _, run := range digitRunRe.FindAllString(key, -1) {
		if run == n {
			return true
		}
	}
	return false
}
