package skills

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"thebot/internal/llm"
	"thebot/internal/prompt"
)

// ============================================================
// One reviewer, with the whole picture
//
// The harness used to answer each live failure with a rule of its own - a pattern for "does not match" in a report, a
// count of files saved but never read, a threshold of unsure verdicts - and the rules came to fight the work: found
// live (GST suite T2, 2026-10-22), a quote-only checker found ten plain calls "unsure", and the operator undid its own
// correct work twice. Code is not where judgement belongs.
//
// So code gathers facts and one independent model call reasons over them, before a result is reported (DONE) and
// before an action the approval gate stopped is put to the person:
//
//   - facts (code, no verdicts): the request and the person's words; every command with what it printed first; the
//     documents the work fetched or copied, and the ones the screens offered; the web pages it looked up; which of the
//     quotes it cites appear in nothing it was shown; its report and the judgement calls it lists;
//   - the reviewer says what is not done, not taken in, contradicted, or not settled by the evidence, and for each how
//     it is settled: from what the work already has (recheck), from general knowledge on the web (lookup), or only by
//     the person (person). It is told not to raise what the evidence plainly settles.
//
// What it raises goes back to the operator once to settle; what is still open goes to the person as one question. A
// call resting only on a web page is put to the person to vouch for the source (knowledge.go). Certain invariants stay
// in code elsewhere (a broken file, a reference copied wrong, a secret, the approval gate); judgement does not.
// ============================================================

const (
	maxReviewLedger      = 150
	maxReviewChars       = 6_000
	maxReviewMemoryChars = 20_000
	reviewTimeoutMs      = 180_000
	maxReviewIssues      = 8
)

// personWords is everything the person said in this goal, with what each reply answered: kept apart from the
// transcript (resume), or read from it for a goal checkpointed before that was kept.
func (st *operatorState) personWords() []string {
	if len(st.PersonSaid) > 0 {
		return st.PersonSaid
	}
	return personSaid(st.Messages)
}

// tailText is the last n bytes of s (from a whole character), marked as cut.
func tailText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := len(s) - n
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return "[... earlier part left out]\n" + s[cut:]
}

// reviewSystem is the reviewer's brief.
const reviewSystem = `You review a computer worker's work before it is reported to the person or acted on, independently of the worker. You are given what it was asked, what the person said, every command it ran with what it printed, the documents it took in and the ones the screens offered, the web pages it looked up, its report and the decisions it says the work rests on, each with what it cites.

Find what would make the result wrong or the report untrue: a part of the request not done; a document it was given, or that a screen offered for the task, that it never took in; a decision that the evidence it cites (and what you can see in the commands' results) does not establish, or that a source contradicts; something the report states that the results contradict; a rule or piece of knowledge the work depends on that nothing it was given provides.

When what it was asked is one stage of a longer request (it says so), judge only that stage: the other stages' parts are done in their own stages and are not missing here - never ask for them to be done now.

The command list shows only the start of what each command printed; the passages show where the words a decision cites stand in the full outputs. Something missing from the command list alone is not missing from what the worker saw.

Do not raise what the evidence plainly settles, wording or style, or how the work was carried out when the result is right. Be specific: name the record, document, field or figure, and what you saw.

For each issue, say how it is settled: "recheck" - the worker can settle it from what it already has or can open; "lookup" - general knowledge that a reliable page on the web can give; "person" - only the person can say (their choice, their rule, their data).

Reply with JSON only: {"issues": [{"what": "...", "why": "...", "settle": "recheck"}]} - an empty list when nothing is wrong.`

// reviewIssue is one thing the reviewer raised.
type reviewIssue struct {
	What   string `json:"what"`
	Why    string `json:"why"`
	Settle string `json:"settle"`
}

// text is the issue as one line.
func (i reviewIssue) text() string {
	line := strings.TrimSpace(i.What)
	if why := strings.TrimSpace(i.Why); why != "" {
		line += " - " + why
	}
	return truncateText(line, 500)
}

// reviewFacts is what the reviewer is shown: facts the goal holds, gathered by code, with no verdict of its own.
func (st *operatorState) reviewFacts(report string, items []string, action string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "THE REQUEST:\n%s\n", truncateText(st.Request, maxReviewChars))
	if said := st.personWords(); len(said) > 0 {
		b.WriteString("\nWHAT THE PERSON SAID IN THIS GOAL (with what each reply answered):\n- " +
			tailText(strings.Join(said, "\n- "), maxReviewMemoryChars) + "\n")
	}
	if prefix := st.Messages[:st.prefixEnd()]; len(prefix) > 0 && prefix[0].Role == "system" {
		// The worker's memory of the person's rules and earlier answers lives in its system frame, after the standing
		// persona every call shares: the persona is left out, and a long frame keeps its end, where the lessons are.
		memory := strings.TrimSpace(strings.TrimPrefix(prefix[0].Content, prompt.Persona))
		b.WriteString("\nWHAT THE WORKER KNOWS FROM EARLIER (its memory, the person's rules and lessons):\n" +
			tailText(memory, maxReviewMemoryChars) + "\n")
	}
	if len(st.LastIssues) > 0 && st.SentBackRound == st.JudgementRounds {
		b.WriteString("\nPOINTS AN EARLIER REVIEW RAISED, SENT BACK TO THE WORKER (its report and the work since may settle " +
			"them; raise one again only if it is still not settled):\n- " + strings.Join(st.LastIssues, "\n- ") + "\n")
	}
	if len(st.Ledger) > 0 {
		shown := st.Ledger
		if len(shown) > maxReviewLedger {
			shown = shown[len(shown)-maxReviewLedger:]
		}
		fmt.Fprintf(&b, "\nCOMMANDS AND WHAT EACH PRINTED FIRST (%d in all, latest last):\n%s\n", len(st.Ledger), strings.Join(shown, "\n"))
	}
	var fetched []string
	for _, c := range st.Commands {
		for _, name := range fetchedDocuments(c) {
			if !contains(fetched, name) {
				fetched = append(fetched, name)
			}
		}
	}
	if len(fetched) > 0 {
		b.WriteString("\nDOCUMENTS THE WORK SAVED OR COPIED: " + strings.Join(fetched, ", ") + "\n")
	}
	if len(st.ScreenDocs) > 0 {
		b.WriteString("\nDOCUMENTS THE SCREENS SHOWED (attachments, lists): " + strings.Join(st.ScreenDocs, ", ") + "\n")
	}
	if len(st.Lookups) > 0 {
		b.WriteString("\nWEB PAGES LOOKED UP (not a source until the person vouches for them):\n")
		for _, l := range st.Lookups {
			b.WriteString("- " + l.Command + " " + strings.Join(l.Links, " ") + "\n")
		}
	}
	if unfound := unbackedCalls(items, st.judgementSources()); len(unfound) > 0 {
		b.WriteString("\nDECISIONS WHOSE CITED WORDS APPEAR IN NO TEXT THE WORKER WAS SHOWN (pictures it looked at are not " +
			"searched), or that say they rest on nothing:\n- " +
			strings.Join(unfound, "\n- ") + "\n")
	}
	if passages := quotedPassages(items, st.judgementSources()); len(passages) > 0 {
		b.WriteString("\nWHERE THE WORDS ITS DECISIONS CITE APPEAR IN WHAT IT WAS SHOWN (the passage around each, from the full outputs):\n- " +
			strings.Join(passages, "\n- ") + "\n")
	}
	if action != "" {
		b.WriteString("\nWHAT IT IS ABOUT TO DO: " + truncateText(action, 600) + "\nThe worker has stopped just before this " +
			"action to ask the person's approval, as it must: it is not done yet, by design. Judge whether the work so far " +
			"and this action are right; do not raise that the action is not done.\n")
	}
	fmt.Fprintf(&b, "\nWHAT IT SAYS (its report, or its reply at the approval):\n%s\n", truncateText(report, maxReviewChars))
	if len(items) > 0 {
		b.WriteString("\nTHE DECISIONS IT SAYS THE WORK RESTS ON:\n- " + strings.Join(items, "\n- ") + "\n")
	}
	return b.String()
}

const (
	// passageChars is how much of what was shown is kept each side of a cited quote.
	passageChars = 300
	// maxPassages bounds the passages shown.
	maxPassages = 16
)

// quotedPassages is, for each quote the decisions cite that is in sources, the passage around it there: the reviewer's
// evidence, since the ledger keeps only the start of each output. Found live (GST suite T3, 2026-10-22): the figures
// were in a screen read's full output, the ledger line held its first words, and the review asked the person whether
// figures the worker had read were ever shown.
func quotedPassages(items []string, sources string) []string {
	plain, offs := plainIndexed(sources)
	var out []string
	for _, item := range items {
		backing := item
		if loc := backedByRe.FindStringIndex(item); loc != nil {
			backing = item[loc[1]:]
		}
		for _, m := range judgementQuoteRe.FindAllStringSubmatch(backing, -1) {
			at, exact := quoteAt(m[1], plain)
			if at < 0 {
				continue
			}
			raw := offs[at]
			lo, hi := max(0, raw-passageChars), min(len(sources), raw+len(m[1])+passageChars)
			for lo > 0 && !utf8.RuneStart(sources[lo]) {
				lo--
			}
			for hi < len(sources) && !utf8.RuneStart(sources[hi]) {
				hi++
			}
			how := "is in"
			if !exact {
				// A short quote found only as its words standing close together: the passage, not the quote, is the fact.
				how = "is not there as quoted; its words stand near each other in"
			}
			out = append(out, fmt.Sprintf("%q %s: ...%s...", truncateText(m[1], 120), how, strings.TrimSpace(sources[lo:hi])))
			if len(out) == maxPassages {
				return out
			}
		}
	}
	return out
}

// plainIndexed is text as quoteAt reads it (plainWords, a space each side) with, for each of its bytes, the offset in
// text it came from.
func plainIndexed(text string) (string, []int) {
	var b strings.Builder
	offs := []int{0}
	b.WriteByte(' ')
	space := true
	for i, r := range text {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			s := string(unicode.ToLower(r))
			b.WriteString(s)
			for range len(s) {
				offs = append(offs, i)
			}
			space = false
		} else if !space {
			b.WriteByte(' ')
			offs = append(offs, i)
			space = true
		}
	}
	if !space {
		b.WriteByte(' ')
		offs = append(offs, len(text))
	}
	return b.String(), offs
}

// reviewTemperature keeps the reviewer steady (0 would be left out of the request as unset and mean the default).
const reviewTemperature = 0.1

// review is the reviewer's issues for the goal as it stands, and whether a review was made: tried twice, a review
// that cannot be made decides nothing but is said (knowledge.go carries it with the result).
func (o *OperatorSkill) review(rc RunContext, st *operatorState, said string, items []string, action string) ([]reviewIssue, bool) {
	if rc.Provider == nil {
		return nil, false
	}
	facts := st.reviewFacts(said, items, action)
	for try := 1; try <= 2; try++ {
		resp, err := rc.Provider.Chat(rc.Ctx, rc.Model, []llm.Message{{Role: "system", Content: reviewSystem},
			{Role: "user", Content: facts}}, llm.Options{TimeoutMs: reviewTimeoutMs, Temperature: reviewTemperature})
		if err != nil {
			rc.Logf("operator", "could not review the work (try %d): %v", try, err)
			continue
		}
		text := resp.Text
		if i, j := strings.Index(text, "{"), strings.LastIndex(text, "}"); i >= 0 && j > i {
			text = text[i : j+1]
		}
		var answer struct {
			Issues *[]reviewIssue `json:"issues"`
		}
		if json.Unmarshal([]byte(text), &answer) != nil || answer.Issues == nil {
			rc.Logf("operator", "the review's answer could not be read (try %d)", try)
			continue
		}
		var out []reviewIssue
		for _, i := range *answer.Issues {
			if strings.TrimSpace(i.What) != "" {
				out = append(out, i)
			}
			if len(out) == maxReviewIssues {
				break
			}
		}
		return out, true
	}
	return nil, false
}
