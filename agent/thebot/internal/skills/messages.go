package skills

import (
	"regexp"
	"strings"
)

// A message the task listed and never opened. Found live (2026-10-26, twice): a client's bills came in one mail, and
// "Revised invoice 0201" from the same sender stood in the same inbox listing, two rows down; the worker entered the
// first mail's bill and the report never named the second. What is decided here is mechanical - a row never opened,
// from a sender the task did open, or naming a number this task's work holds - never what the row's words mean
// (review T-18: a list of "revised/cancel/updated" words decided which rows counted).

// listRowRe is a row of a list on the screen as the agent prints it: e12 row "Sender - Subject | preview | date".
var listRowRe = regexp.MustCompile(`(?m)\b(e\d+) row "([^"\n]{8,400})"`)

// clickedRefRe is a click on a ref in a command.
var clickedRefRe = regexp.MustCompile(`\bgui (?:click|double_click|open) (e\d+)\b`)

// digitRunRe is a run of three or more digits (a document number).
var digitRunRe = regexp.MustCompile(`\d{3,}`)

// maxUnopenedMessages bounds the points raised for messages never opened.
const maxUnopenedMessages = 3

// listedMessage is one row of a message list seen in the task: who it is from, its subject, the whole row, and the
// refs it was listed under.
type listedMessage struct {
	sender, subject, text string
	refs                  []string
}

// listedMessages are the rows of message lists the task saw, by sender and subject.
func (st *operatorState) listedMessages() []*listedMessage {
	var out []*listedMessage
	byKey := map[string]*listedMessage{}
	for _, l := range st.Listings {
		for _, m := range listRowRe.FindAllStringSubmatch(l, -1) {
			text := m[2]
			i := strings.Index(text, " - ")
			if i <= 0 {
				continue
			}
			sender, rest := strings.TrimSpace(text[:i]), text[i+3:]
			subject := rest
			if j := strings.Index(rest, " | "); j > 0 {
				subject = rest[:j]
			}
			subject = strings.TrimSpace(subject)
			if sender == "" || subject == "" {
				continue
			}
			key := strings.ToLower(sender + " - " + subject)
			row := byKey[key]
			if row == nil {
				row = &listedMessage{sender: sender, subject: subject, text: text}
				byKey[key] = row
				out = append(out, row)
			}
			row.refs = append(row.refs, m[1])
		}
	}
	return out
}

// opened reports whether a listed message was opened: one of its refs was clicked, or its subject stands on a page
// as a heading or text of its own (not a row of the list).
func (st *operatorState) opened(row *listedMessage) bool {
	clicked := map[string]bool{}
	for _, c := range st.Worked {
		for _, m := range clickedRefRe.FindAllStringSubmatch(c, -1) {
			clicked[m[1]] = true
		}
	}
	for _, r := range row.refs {
		if clicked[r] {
			return true
		}
	}
	subject := strings.ToLower(row.subject)
	seen := strings.ToLower(st.Seen)
	return strings.Contains(seen, `heading "`+subject) || strings.Contains(seen, `text "`+subject)
}

// aboutThisTask reports whether a number in a message row (a run of three or more digits) is one this task's work
// holds: a command that worked, or the record ledger.
func (st *operatorState) aboutThisTask(row *listedMessage) bool {
	worked := strings.Join(st.Worked, "\n") + "\n" + st.RecordLedger
	for _, run := range digitRunRe.FindAllString(row.text, -1) {
		if strings.Contains(worked, run) {
			return true
		}
	}
	return false
}

// unopenedMessageIssues are the points for messages the task listed and never opened: one from a sender whose other
// message the task opened, or one that names a number this task's work holds - unless the report names it. Each is the worker's to do (reviewIssue.Work): open it, or say why it is not this task's.
func (st *operatorState) unopenedMessageIssues(report string) []reviewIssue {
	rows := st.listedMessages()
	if len(rows) == 0 {
		return nil
	}
	openedSenders := map[string]bool{}
	openedRows := map[*listedMessage]bool{}
	for _, r := range rows {
		if st.opened(r) {
			openedRows[r] = true
			openedSenders[strings.ToLower(r.sender)] = true
		}
	}
	said := strings.ToLower(report)
	var out []reviewIssue
	for _, r := range rows {
		if openedRows[r] {
			continue
		}
		if !openedSenders[strings.ToLower(r.sender)] && !st.aboutThisTask(r) {
			continue
		}
		if strings.Contains(said, strings.ToLower(r.subject)) {
			continue // named in the report, with why it is left out or what was done
		}
		out = append(out, reviewIssue{
			What: "A listed message was never opened: \"" + r.sender + " - " + r.subject + "\"",
			Why: "a later message from the same sender can revise, cancel or add to an earlier document; nothing of " +
				"this task has read it",
			Settle: "open it and take in what it says, or name it in the report with why it is not this task's",
			Work:   true,
		})
		if len(out) == maxUnopenedMessages {
			break
		}
	}
	return out
}
