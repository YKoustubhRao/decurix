package skills

import (
	"fmt"
	"regexp"
	"strings"
)

// ============================================================
// The judgement calls a result rests on
//
// Found live (GST suite, 2026-10-22): the misses of a strong hosted model were not questions it could not answer but
// decisions it never noticed it made - rent under reverse charge also listed as a purchase credit (counted twice),
// "0 cancelled" for a month with a cancelled bill, a figure read off another month's block. Each was a judgement call
// the request, the lesson and the documents did not back, made silently.
//
// So, for a hosted model, before a result is reported (DONE) and before an action the approval gate stops (filing,
// submitting, sending, paying) is put to the person, the operator lists the judgement calls the result rests on, each
// with what backs it - the words of the request, the person's answers or lesson, or the line of a document or screen
// it read - quoted. Code then checks each quote against everything the goal has been shown (st.Seen): a call whose
// backing is not there, or that says it is not backed, is not the worker's to make alone, and goes to the person as
// one question (their answer is kept as a lesson for later tasks by the agent, lessons.py). The approval the person is
// asked for carries the list, so a yes is given knowing what it rests on.
//
// Nothing here knows any domain: what counts as a call is the model's account, and only whether its backing exists is
// decided in code.
// ============================================================

const (
	// maxJudgementRounds bounds the rounds of one goal; minJudgementCommands keeps a quick question from having any.
	maxJudgementRounds   = 4
	minJudgementCommands = 4
	// minQuoteWords is the shortest stretch of a quote that, found in what was seen, backs it.
	minQuoteWords = 5
)

var (
	// judgementsHeadRe opens the list: "JUDGEMENTS:" (or JUDGMENTS, bold or not).
	judgementsHeadRe = regexp.MustCompile(`(?im)^[ \t>*_#-]*JUDG(?:E)?MENTS?[ \t*_]*:[ \t]*(.*)$`)
	// judgementItemRe is one item of the list.
	judgementItemRe = regexp.MustCompile(`^\s*(?:[-*\x{2022}]|\d{1,2}[.)])\s+(.+)$`)
	// judgementQuoteRe is a quoted backing: straight or curly double quotes.
	judgementQuoteRe = regexp.MustCompile(`["\x{201c}\x{201d}]([^"\x{201c}\x{201d}]{6,600})["\x{201c}\x{201d}]`)
	// notBackedRe is an item that says it rests on nothing given.
	notBackedRe = regexp.MustCompile(`(?i)\bnot backed\b|\bunbacked\b|\bno (?:source|backing)\b|\bmy (?:own )?(?:assumption|guess|choice)\b`)
	// backedByRe is where an item's backing starts.
	backedByRe = regexp.MustCompile(`(?i)\bbacked by\s*:`)
	// controlLineRe is the start of a marker after the list.
	controlLineRe = regexp.MustCompile(`(?im)^\s*(?:DONE|COMMAND|HELP)\s*:`)
	nonWordRe     = regexp.MustCompile(`[^\p{L}\p{N}]+`)
)

// judgementList is the list in a reply: its items, and whether the reply has one at all ("JUDGEMENTS: none" is a list
// with no items).
func judgementList(text string) ([]string, bool) {
	loc := judgementsHeadRe.FindStringSubmatchIndex(text)
	if loc == nil {
		return nil, false
	}
	rest := text[loc[1]:]
	if m := controlLineRe.FindStringIndex(rest); m != nil {
		rest = rest[:m[0]]
	}
	var items []string
	if first := strings.Trim(text[loc[2]:loc[3]], " \t*_"); first != "" && !regexp.MustCompile(`(?i)^none\b`).MatchString(first) {
		items = append(items, first)
	}
	blank := false
	for _, line := range strings.Split(rest, "\n") {
		if strings.TrimSpace(line) == "" {
			blank = len(items) > 0
			continue
		}
		if m := judgementItemRe.FindStringSubmatch(line); m != nil {
			items = append(items, strings.TrimSpace(m[1]))
			blank = false
			continue
		}
		if blank {
			break // the report goes on after the list
		}
		if len(items) > 0 && strings.HasPrefix(line, " ") {
			items[len(items)-1] += " " + strings.TrimSpace(line) // an item carried onto the next line
		}
	}
	return items, true
}

// plainWords is text as words to compare: lower case, letters and digits only.
func plainWords(text string) string {
	return strings.TrimSpace(nonWordRe.ReplaceAllString(strings.ToLower(text), " "))
}

// quoteSeen reports whether a quote's words are in what was seen: whole, or a stretch of minQuoteWords of them (a
// quote may run two cells or lines together, or drop a word).
func quoteSeen(quote, seen string) bool {
	q := plainWords(quote)
	if q == "" {
		return false
	}
	if strings.Contains(seen, q) {
		return true
	}
	words := strings.Fields(q)
	if len(words) < minQuoteWords {
		// A short quote - a cell and its value ("O62='Karnataka'"), a code and a name - is backed when all its words
		// stand close together in what was seen: office read writes a cell inside its formula and beside its value, not
		// as the quote does (found live, GST suite T5, 2026-10-22).
		return wordsNear(words, seen, nearChars)
	}
	for i := 0; i+minQuoteWords <= len(words); i++ {
		if strings.Contains(seen, strings.Join(words[i:i+minQuoteWords], " ")) {
			return true
		}
	}
	return false
}

// nearChars is how close the words of a short quote must stand in what was seen.
const nearChars = 240

// wordsNear reports whether every word stands, as a whole word, within nearChars of an occurrence of the first.
func wordsNear(words []string, seen string, within int) bool {
	first := " " + words[0] + " "
	for at := 0; ; {
		i := strings.Index(seen[at:], first)
		if i < 0 {
			return false
		}
		i += at
		lo, hi := max(0, i-within), min(len(seen), i+len(first)+within)
		window := seen[lo:hi]
		all := true
		for _, w := range words[1:] {
			if !strings.Contains(window, " "+w+" ") {
				all = false
				break
			}
		}
		if all {
			return true
		}
		at = i + 1
	}
}

// unbackedCalls are the items whose backing is not in what was seen: none quoted, none of the quotes there, or the
// item says it rests on nothing given.
func unbackedCalls(items []string, seen string) []string {
	plain := " " + plainWords(seen) + " "
	var out []string
	for _, item := range items {
		if notBackedRe.MatchString(item) {
			out = append(out, item)
			continue
		}
		// The backing is what follows "backed by:" - a quote earlier in the call ("used it as "my PC" ...") would
		// otherwise pair its closing mark with the backing's opening one (found live, GST suite T1, 2026-10-22).
		backing := item
		if loc := backedByRe.FindStringIndex(item); loc != nil {
			backing = item[loc[1]:]
		}
		backed := false
		for _, m := range judgementQuoteRe.FindAllStringSubmatch(backing, -1) {
			if quoteSeen(m[1], plain) {
				backed = true
				break
			}
		}
		if !backed {
			out = append(out, item)
		}
	}
	return out
}

// judgementSources is what a call may be backed by: everything the goal was shown, and its fixed part - the system
// frame with the worker's memory of earlier lessons, and the contract with the request and what the person taught.
func (st *operatorState) judgementSources() string {
	var b strings.Builder
	b.WriteString(st.Seen)
	for _, m := range st.Messages[:st.prefixEnd()] {
		b.WriteString("\n" + m.Content)
	}
	return b.String()
}

// judgementsDue is whether a round is due now: hosted, rounds left, a goal that did real work, and work done since the
// last round.
func (st *operatorState) judgementsDue() bool {
	return st.Hosted && st.AwaitingJudge == "" && st.JudgementRounds < maxJudgementRounds &&
		st.CommandsSucceeded >= minJudgementCommands && (st.JudgementRounds == 0 || st.CommandsSucceeded > st.JudgedAt)
}

// judgementsHow is what the operator is told a judgement call is and how to list one.
const judgementsHow = "A judgement call is a decision that changes what the result says or holds: how you treated, " +
	"classified, counted, included or left out something; which of two readings of a document, a rule or a layout you " +
	"took; which record, row, period or block a figure came from; what a value means. Not the mechanics - which tool " +
	"or command, how you got or saved a file, a retry, a path - and not what the request plainly says. For each, say what backs it by " +
	"quoting the words exactly, in double quotes: the request, the person's answers or what they taught, or the line " +
	"of the document, sheet or screen you read it from (with where). Write them as:\n" +
	"JUDGEMENTS:\n" +
	"- <the call> | backed by: \"<the exact words>\" (<where>)\n" +
	"- <the call> | not backed: <why you chose it>\n" +
	"A call may rest on a web page you looked up (lookup \"...\"): quote it and give its link - the person is asked " +
	"whether to trust that source. Where you do not know how something is done (a rule, a convention, what a field " +
	"means), do not guess: look it up, or ask the person (HELP) for the rule, or for documentation or a link. " +
	"If you find a call is wrong while listing it, fix the work first. If there were none, write JUDGEMENTS: none."

// judgementsDoneNudge is said before a result is reported.
const judgementsDoneNudge = "Before this is reported, check what the result rests on. " + judgementsHow +
	"\nThen reply DONE again with your report, ending with the JUDGEMENTS list."

// judgementsApprovalNudge is said before an action the gate stopped is put to the person.
func judgementsApprovalNudge(reason string) string {
	return fmt.Sprintf("Before this is put to the person for approval (%s), check what it rests on - once approved it "+
		"cannot be taken back. %s\nReply with the JUDGEMENTS list only, no command: the action is then put to the "+
		"person with it. If you want to change something first, reply with that COMMAND instead.", truncateText(reason, 300),
		judgementsHow)
}

// judgementsForApproval is the list as the approval shows it.
func judgementsForApproval(items []string) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nWhat this rests on (the worker's judgement calls):")
	for _, item := range items {
		b.WriteString("\n- " + truncateText(item, 300))
	}
	return b.String()
}
