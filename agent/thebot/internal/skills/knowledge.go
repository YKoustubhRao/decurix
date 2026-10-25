package skills

import (
	"fmt"
	"regexp"
	"strings"
)

// ============================================================
// When the work is not sure: settle it from a source, the web or the person
//
// What the work rests on is weighed by one reviewer with the whole picture (reviewer.go); what it raises goes back to
// the operator once - to settle from what it has, from the web, or by asking - and what stays open goes to the person
// as one question. This file holds what the person's word decides:
//   - a web page the goal looked up is never a source of the work until the person vouches for it: a call resting
//     only on one is asked as "is this a source you trust for this?", with its link; anything but a no makes the page
//     a source for the goal (settleAskedSources), and their answer is kept as a lesson by the agent (lessons.py);
//   - a link the person gives (the request, a reply) is theirs: what the work reads there is a source at once.
// ============================================================

const (
	maxLookups     = 12
	maxLookupChars = 30_000
	maxPersonLinks = 50
)

// webLookup is one web page or search the goal read: the command, the links its output names, and its text.
type webLookup struct {
	Command string
	Links   []string
	Text    string
}

// linkRe is a web link.
var linkRe = regexp.MustCompile(`https?://[^\s"'<>)\]]+`)

// noteLookup keeps what a web lookup printed, apart from what the goal has seen as sources.
func (st *operatorState) noteLookup(command, output string) {
	links := linkRe.FindAllString(command+"\n"+output, 8)
	st.Lookups = append(st.Lookups, webLookup{Command: truncateText(command, 400), Links: links, Text: truncateText(output, maxLookupChars)})
	if len(st.Lookups) > maxLookups {
		st.Lookups = st.Lookups[len(st.Lookups)-maxLookups:]
	}
}

// notePersonLinks keeps the links the person gave: what the work reads there is theirs.
func (st *operatorState) notePersonLinks(text string) {
	for _, l := range linkRe.FindAllString(text, -1) {
		l = strings.TrimRight(l, ".,;:")
		if !contains(st.PersonLinks, l) && len(st.PersonLinks) < maxPersonLinks {
			st.PersonLinks = append(st.PersonLinks, l)
		}
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// fetchesPersonLink reports whether a command reads a page the person linked.
func (st *operatorState) fetchesPersonLink(command string) bool {
	for _, l := range st.PersonLinks {
		if strings.Contains(command, l) {
			return true
		}
	}
	return false
}

// settleAskedSources takes the person's reply to "is this a source you trust?": a reply that accepts them (trusted,
// replies.go) makes the pages sources for the rest of the goal - unless it gives a link of its own (the source to
// follow instead).
func (st *operatorState) settleAskedSources(reply string, trusted bool) {
	if len(st.AskedSources) == 0 {
		return
	}
	if trusted && !linkRe.MatchString(reply) {
		for _, i := range st.AskedSources {
			if i >= 0 && i < len(st.Lookups) {
				st.noteSeen(st.Lookups[i].Text)
			}
		}
	}
	st.AskedSources = nil
}

// lookupBacking is the web page an item's quote comes from, as (index, link), or (-1, "") when none holds it.
func (st *operatorState) lookupBacking(item string) (int, string) {
	backing := item
	if loc := backedByRe.FindStringIndex(item); loc != nil {
		backing = item[loc[1]:]
	}
	for _, m := range judgementQuoteRe.FindAllStringSubmatch(backing, -1) {
		for i := len(st.Lookups) - 1; i >= 0; i-- {
			if quoteSeen(m[1], " "+plainWords(st.Lookups[i].Text)+" ") {
				link := ""
				if len(st.Lookups[i].Links) > 0 {
					link = st.Lookups[i].Links[0]
				}
				return i, link
			}
		}
	}
	return -1, ""
}

// maxReviewQuestions bounds the questions the review puts to the person in one goal; past it, what is still open goes
// with the result or the approval, never silently dropped.
const maxReviewQuestions = 2

// weighing is what follows the work's account of what it rests on: a message back to the operator, a question for the
// person, or neither - with open the points still unsettled once the person has been asked as often as they may be,
// shown with the result or the approval.
type weighing struct {
	nudge, question string
	open            []string
}

// unreviewed is what stands for a review that could not be made.
const unreviewed = "An independent review of this work could not be made, so nothing beyond the worker's own checks backs it."

// weighJudgements decides what follows the work's own account of what it rests on (items, possibly none; said, the
// reply that gave them). action is what the approval is for, "" at a DONE. The review reasons (reviewer.go); code
// only adds that a call resting on a web page needs the person's word on the source. Per round, what the review raises
// goes back to the operator once; what is still open then is asked (up to maxReviewQuestions a goal) or, past that,
// carried with the result.
func (o *OperatorSkill) weighJudgements(rc RunContext, st *operatorState, items []string, said, action string) weighing {
	var web []string
	var webIdx []int
	for _, item := range unbackedCalls(items, st.judgementSources()) {
		if i, link := st.lookupBacking(item); i >= 0 && !notBackedRe.MatchString(item) {
			if link != "" {
				item += " - from " + link
			}
			web = append(web, item)
			webIdx = append(webIdx, i)
		}
	}
	// Values only a picture showed are read a second time first (second_read.go): what both reads agree on is traced
	// before the review looks, and what they do not is a point like the review's.
	pictured := o.secondRead(rc, st)
	issues, reviewed := o.review(rc, st, said, items, action)
	issues = append(issues, pictured...)
	if action == "" {
		// Before a report: a document of the work nothing written reflects, and the report never names, is a point -
		// decided in code, not left to the review to notice (evidence.go).
		issues = append(issues, st.coverageIssues(said)...)
		// ... and a plan with steps for the person's OK of which none went through (plan.go).
		issues = append(issues, st.commitsIssue()...)
		// ... and a one-word value written into a file that no source holds (evidence.go noteInventedCodes).
		issues = append(issues, st.inventedCodeIssues(said)...)
		// The figures the work leaves on a screen it typed into are worked out again from the sources, as before a
		// commit: a "prepare it, do not file it" task reaches no commit. Found live (2026-10-25): a service line was
		// folded into a goods line of a summary; the totals tied, nothing warned, and the check passed it.
		// Every page typed into is worked out, not only the last (verifyReport); one typed into after its last look is
		// looked at again first (unseenTyping).
		if typedOnScreen(st) {
			issues = append(issues, o.verifyReport(rc, st)...)
			issues = append(issues, st.unseenTyping()...)
		}
	}
	if action != "" {
		// Before an action that cannot be taken back, two independent checks: the screen's figures worked out again from
		// the sources by a check that does not see them (evidence.go), and the records the action covers against every
		// source read, each disagreement needing a source that explains it (case.go). Their findings are points like
		// the review's.
		o.lookBeforeCommit(rc, st, action)
		issues = append(issues, o.verifyCommit(rc, st, action)...)
		issues = append(issues, o.caseMatrix(rc, st, action)...)
	}
	var texts []string
	for _, i := range issues {
		texts = append(texts, i.text())
	}

	// Once a round - and again when the work changed something since the last send-back: points found in new work are
	// the worker's to settle first. Found live (2026-10-25): sent back once early in a round, the worker then keyed a
	// whole return, with three answers from the person in between; at its report the verifier found six wrong figures
	// in one row, and they went straight to the person - the worker never saw them.
	again := st.SentBackRound == st.JudgementRounds && st.changedSinceSentBack() && st.ExtraSendBacks < maxExtraSendBacks
	if len(issues) > 0 && (st.SentBackRound != st.JudgementRounds || again) {
		if again {
			st.ExtraSendBacks++
		}
		st.SentBackRound, st.SentBackWorked, st.LastIssues = st.JudgementRounds, len(st.Worked), texts
		rc.Logf("operator", "the review raised %d point(s) - sent back to settle them", len(issues))
		return weighing{nudge: settleNudge(issues)}
	}
	st.LastIssues = texts
	// Before an action the gate stopped, what is still open goes into the approval itself - one question for one
	// action: asked first as a question and then for the approval, every gated click cost the person two
	// (2026-10-23). Their no, with what to do instead, comes back like any refusal.
	// A point the worker could settle by looking ("recheck") and did not is asked too: carried silently with the result
	// instead, it left a month's work wrong with no word from the person, whose answer (their rules) had settled the same
	// points the run before (2026-10-23: 32/46 against 45/46). Correctness before fewer questions.
	if (len(issues) > 0 || len(web) > 0) && action == "" && st.PersonAsks < maxReviewQuestions {
		st.PersonAsks++
		st.JudgementQuestion = true
		st.AskedSources = webIdx
		st.AskedPoints = append(append([]string(nil), texts...), web...)
		rc.Logf("operator", "%d point(s) still open, %d resting on the web - asking the person", len(issues), len(web))
		return weighing{question: st.withoutSecrets(knowledgeQuestion(issues, web, action))}
	}
	open := append([]string(nil), texts...)
	for _, w := range web {
		open = append(open, "rests on a web page the person has not vouched for: "+w)
	}
	if !reviewed {
		open = append(open, unreviewed)
	}
	if len(open) > 0 {
		rc.Logf("operator", "%d point(s) still open - carried with the result", len(open))
	}
	return weighing{open: open}
}

// maxExtraSendBacks bounds the send-backs a goal makes for points found in work done since the last one (knowledge.go).
const maxExtraSendBacks = 2

// changedSinceSentBack reports whether a command that changes something (changesSomething) went through since points
// were last sent back: only reading to settle them is not new work.
func (st *operatorState) changedSinceSentBack() bool {
	from := st.SentBackWorked
	if from > len(st.Worked) {
		from = len(st.Worked)
	}
	for _, c := range st.Worked[from:] {
		if changesSomething(c) {
			return true
		}
	}
	return false
}

// typedOnScreenRe is a gui command that types or chooses on the screen.
var typedOnScreenRe = regexp.MustCompile(`(^|&&|;|\s)gui\s+(type|select)\b`)

// typedOnScreen reports whether the goal typed or chose anything on the screen (gui type, gui select) that went through.
func typedOnScreen(st *operatorState) bool {
	for _, c := range st.Worked {
		if typedOnScreenRe.MatchString(c) {
			return true
		}
	}
	return false
}

// openPointsNote is what stays open, as the result or the approval shows it.
func openPointsNote(st *operatorState, open []string) string {
	if len(open) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nStill open after an independent review of the work - please check:")
	for _, p := range open {
		b.WriteString("\n- " + truncateText(p, 500))
	}
	return st.withoutSecrets(b.String())
}

// reviewInProgress ends a review round a gate's stop interrupts (a command run while settling what the review raised):
// the approval then carries the points still open, never asked without them.
func (st *operatorState) reviewInProgress() string {
	if st.AwaitingJudge == "" {
		return ""
	}
	open := st.LastIssues
	st.AwaitingJudge, st.PendingReason, st.Settling = "", "", false
	return openPointsNote(st, open)
}

// withoutSecrets is text with every secret the request gave replaced (what the harness writes for the person never
// carries one; the worker's own words are withheld by the host).
func (st *operatorState) withoutSecrets(text string) string {
	for secret := range secretValues(st, "") {
		if len(secret) >= 3 {
			text = strings.ReplaceAll(text, secret, secretShown)
		}
	}
	return text
}

// settleNudge sends what the review raised back to the operator, once.
func settleNudge(issues []reviewIssue) string {
	var b strings.Builder
	b.WriteString("An independent review of the work raised these points:\n")
	for n, i := range issues {
		how := map[string]string{
			"recheck": "settle it from what you have: open the record, document or file and read it",
			"lookup":  "look it up (lookup \"...\") and quote the page, with its link - the person will be asked whether to trust that source",
			"person":  "ask the person (HELP) for their rule or choice, or for documentation or a link",
		}[strings.ToLower(strings.TrimSpace(i.Settle))]
		if how == "" {
			how = "settle it from a source, the web or the person"
		}
		fmt.Fprintf(&b, "%d. %s\n   -> %s\n", n+1, i.text(), how)
	}
	b.WriteString("Weigh each point yourself - the review can be wrong; where what you have shows it is, say so with the " +
		"words that show it. Keep the work that is right; change only what a source shows to be wrong. Then reply again " +
		"(DONE with your report and the JUDGEMENTS list, or the JUDGEMENTS list alone before an approval)." + wholeReportNote)
	return b.String()
}

// knowledgeQuestion is what the person is asked, each point as what it is: web sources to vouch for, choices only they
// settle, knowledge nothing given provides, and points an independent check still questions. Numbered through.
func knowledgeQuestion(issues []reviewIssue, web []string, action string) string {
	var choices, knowledge, checks []string
	for _, i := range issues {
		switch strings.ToLower(strings.TrimSpace(i.Settle)) {
		case "recheck":
			checks = append(checks, i.text())
		case "lookup":
			knowledge = append(knowledge, i.text())
		default:
			choices = append(choices, i.text())
		}
	}
	var b strings.Builder
	if action != "" {
		fmt.Fprintf(&b, "Before I go on to %s:\n", truncateText(strings.TrimPrefix(action, "on screen it would "), 200))
	} else {
		b.WriteString("Before I report this as done:\n")
	}
	n := 0
	section := func(head string, points []string) {
		if len(points) == 0 {
			return
		}
		b.WriteString(head + "\n")
		for _, p := range points {
			n++
			fmt.Fprintf(&b, "%d. %s\n", n, truncateText(p, 400))
		}
	}
	section("These rest on what I looked up on the web. Is each a source you trust for this? Reply yes to keep them, "+
		"or send me the documentation or link I should follow instead:", web)
	section("These are choices I made that nothing you gave me or taught me settles. Tell me how each should be "+
		"treated, or send me documentation or a link for it, or reply \"as you did\" to keep them:", choices)
	section("These need knowledge that nothing you gave me or taught me provides. Tell me the rule, or send me "+
		"documentation or a link, or reply \"as you did\" to keep what I did:", knowledge)
	section("An independent check of the work still questions these after I went back over them. Tell me if any is "+
		"wrong, or reply \"as you did\" to keep the work as it is:", checks)
	return strings.TrimSpace(b.String())
}
