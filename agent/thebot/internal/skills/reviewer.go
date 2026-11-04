package skills

import (
	"encoding/json"
	"errors"
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
// live (2026-10-22), a quote-only checker found ten plain calls "unsure", and the operator undid its own
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

// maxReviewPictures bounds the pictures the reviewer is shown.
const maxReviewPictures = 3

// lastPictures are the last n pictures still in the transcript (screenshots, scanned pages), oldest first, with their
// labels; none for a model that cannot see.
func (st *operatorState) lastPictures(n int) ([][]byte, []string) {
	if !st.Vision {
		return nil, nil
	}
	var imgs [][]byte
	var labels []string
	for i := len(st.Messages) - 1; i >= 0 && len(imgs) < n; i-- {
		m := st.Messages[i]
		for j := len(m.Images) - 1; j >= 0 && len(imgs) < n; j-- {
			imgs = append([][]byte{m.Images[j]}, imgs...)
			labels = append([]string{m.ImageLabel(j)}, labels...)
		}
	}
	return imgs, labels
}

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

The command list shows what each command printed when it was short, and otherwise its start and how many more lines there were; the passages show where the words a decision cites stand in the full outputs. Something missing from the command list alone is not missing from what the worker saw.

Raise a point only when you can name what is wrong or missing: a contradiction between what the work says and a source, a document or part of the request not handled, a rule the person gave not followed, knowledge the result depends on that nothing provides. That something is not proven by the excerpts you are shown is not enough on its own when nothing points the other way: the worker read whole files and screens you see only in part.

Every instruction the person gave in this goal - check this, look there, fix that first, before you file - is an obligation of the work: raise one the commands show was not carried out, or was carried out another way than they said (name the instruction and what the commands did instead), before the action it was for.

A standing or taught rule about how to go about the work (check the list before entering, look in the folder before copying, match by one field before another) is there for an outcome: nothing entered twice, nothing overwritten, the right record used. When that outcome can be read in what you are shown, judge the outcome - raise the rule only when its outcome is wrong, or when nothing shows the outcome either way.

Work added to a record that already holds earlier entries of the same kind follows them: they are the person's own precedent for its layout. A new entry that departs from that precedent - its columns, how one document is spread over rows, where it sits - is a point: name the rows, what they hold, and the earlier entry they should look like.

What the person has already answered is settled: a point they decided, or left to the worker ("as you did", "use your judgement"), is never raised again unless the work since goes against their answer.

Do not raise what the evidence plainly settles, wording or style, or how the work was carried out when the result is right. Be specific: name the record, document, field or figure, and what you saw.

For each issue, say how it is settled: "recheck" - the worker can settle it from what it already has or can open, or do it (a part of the work not done yet); "lookup" - general knowledge that a reliable page on the web can give; "person" - only the person can say (their choice, their rule, their data).

Reply with JSON only: {"issues": [{"what": "...", "why": "...", "settle": "recheck"}]} - an empty list when nothing is wrong.`

// reviewIssue is one thing the reviewer raised.
type reviewIssue struct {
	What   string `json:"what"`
	Why    string `json:"why"`
	Settle string `json:"settle"`
	// Work marks a point decided in code that is the worker's to do (save the record, name the document), never a
	// choice for the person (knowledge.go).
	Work bool `json:"-"`
	// Advisory marks a point the worker is told once and that never goes to the person or the result: a verifier
	// disagreement on a screen the work did not type into (evidence.go verifyReport).
	Advisory bool `json:"-"`
	// Unchecked marks a check that could not be made (notChecked): never advisory, so it reaches the person.
	Unchecked bool `json:"-"`
}

// errCutOff is a model's answer that stopped for want of room (llm.Result.Truncated): what came is not the whole
// answer, and is never taken for one.
var errCutOff = errors.New("the model's answer was cut off before it was finished")

// cutOffNote is said after a reply shown although it was cut off.
const cutOffNote = "\n\n(This reply was cut off before it was finished: it is incomplete.)"

// notChecked is the point that stands for an independent check that could not be made (the model call failed - a
// timeout, a 429 - or its answer could not be read): what was not checked, never a pass. It goes back to the worker
// like any recheck point, and on to the person with the approval or the result.
func notChecked(what string, err error) reviewIssue {
	why := "the check's answer could not be read"
	if err != nil {
		why = "the check could not be made (" + truncateText(err.Error(), 160) + ")"
	}
	return reviewIssue{
		What:   "Not checked: " + what + " - " + why,
		Why:    "check them against the documents before going on, or say plainly that they are not checked",
		Settle: "recheck", Unchecked: true,
	}
}

// text is the issue as one line.
func (i reviewIssue) text() string {
	line := strings.TrimSpace(i.What)
	if why := strings.TrimSpace(i.Why); why != "" {
		line += " - " + why
	}
	return forPerson(line, maxPointChars)
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
	// What code found about the evidence (evidence.go): facts, each for the reviewer to weigh.
	if len(st.RecordFindings) > 0 {
		b.WriteString("\nRECORDS WHOSE VALUES STAND TOGETHER IN NO SINGLE SOURCE RECORD (a value may have been taken from " +
			"the wrong record, or written from memory):\n- " + strings.Join(st.RecordFindings, "\n- ") + "\n")
	}
	if len(st.PictureOnly) > 0 {
		b.WriteString("\nVALUES TYPED THAT NO TEXT HOLDS, OCR OF THE PICTURES INCLUDED (read off a picture, or not read " +
			"at all): " + strings.Join(st.PictureOnly, ", ") + "\n")
	}
	if len(st.ComputedTyped) > 0 {
		b.WriteString("\nVALUES TYPED THAT ONLY CODE THE WORK WROTE ITSELF PRINTED (its own arithmetic or text - a fact, no " +
			"verdict: check each against the sources it was worked from): " + strings.Join(st.ComputedTyped, ", ") + "\n")
	}
	if len(st.UntracedWrites) > 0 {
		b.WriteString("\nVALUES WRITTEN INTO FILES THAT NO TEXT THE WORK WAS SHOWN HOLDS (read off a picture, worked out in " +
			"the head, or not read at all - check each against its source): " + strings.Join(st.UntracedWrites, ", ") + "\n")
	}
	if n := len(st.Listings); n > 0 {
		// The newest listing of each window the work looked at: a list of messages with a row never opened, a table
		// with a line the report never mentions, is a fact the review can see for itself (found live, 2026-10-26: a
		// revised bill sat in a second message from the client, listed on the inbox page and never opened).
		var shown []string
		for i := n - 1; i >= 0 && len(shown) < maxReviewListings; i-- {
			shown = append([]string{truncateText(st.Listings[i], maxReviewListingChars)}, shown...)
		}
		b.WriteString("\nTHE NEWEST LISTING OF EACH WINDOW THE WORK LOOKED AT (what the screens showed; a message, row or " +
			"document listed here that the work never opened or the report never accounts for is a point):\n" +
			strings.Join(shown, "\n\n") + "\n")
	}
	if strings.TrimSpace(st.RecordLedger) != "" {
		b.WriteString("\nTHE RECORD LEDGER (each record the worker declared from its document before typing it, the values " +
			"it meant to enter, and whether the page showed each after the save - 'seen' is a fact, 'ABSENT' is not " +
			"yet shown; a record entered without a declaration is not listed here - judge it from the other facts):\n" +
			truncateText(st.RecordLedger, maxReviewLedgerChars) + "\n")
	}
	if facts := rateLineFacts(st.RecordLedger); len(facts) > 0 {
		b.WriteString("\nRATE LINES OF THE LEDGER WHOSE SHARE MATCHES NO AMOUNT OF THEIR RECORD (arithmetic, no verdict: a " +
			"figure may be misread, or the line charged on something the record does not hold - judge it with the " +
			"document):\n- " + strings.Join(facts, "\n- ") + "\n")
	}
	if strings.Contains(st.Seen, "=== HANDOFF ") {
		if facts := handoffFacts(st.Seen); len(facts) > 0 {
			b.WriteString("\nPARTS HANDED TO OTHER WORKERS THAT CAME BACK NOT VERIFIED (a report that rests on one as done " +
				"claims what nobody saw):\n- " + strings.Join(facts, "\n- ") + "\n")
		}
	}
	if len(st.folderFacts) > 0 {
		b.WriteString("\nFOLDERS THIS TASK NAMED WHOSE NAME DIFFERS FROM THE SHAPE THE FOLDERS BESIDE THEM SHARE (a fact, " +
			"no verdict: whether it should follow them is yours to judge):\n- " + strings.Join(st.folderFacts, "\n- ") + "\n")
	}
	if gaps := st.coverageGaps(); len(gaps) > 0 {
		b.WriteString("\nDOCUMENTS OF THE WORK THAT NOTHING ENTERED OR WRITTEN REFLECTS (right only when the report says " +
			"why each is left out):\n- " + strings.Join(gaps, "\n- ") + "\n")
	}
	if len(st.AnsweredPoints) > 0 {
		// Found live (2026-10-23): answered "use your judgement", the same point came back reworded.
		b.WriteString("\nPOINTS ALREADY PUT TO THE PERSON, WITH THEIR ANSWER (settled: never raise any of these again, " +
			"reworded or about the same record, unless the work since goes against their answer):\n" +
			strings.Join(st.AnsweredPoints, "\n") + "\n")
	}
	if plan := st.planText(); plan != "" {
		b.WriteString("\nTHE PLAN OF THE WORK (the worker's, written before it changed anything, with the person's answer; a " +
			"place the plan or the person names that the commands never read is a point):\n" + truncateText(plan, 3000) + "\n")
		if st.Plan != nil && len(st.Plan.Commits) > 0 && action == "" {
			// Which of the plan's steps for the person's OK went through: one that did not, at a report, is a point
			// (found live 2026-10-24: the returns were filed and the message to the client the plan named never sent).
			done := "none"
			if len(st.CommitsDone) > 0 {
				done = "\n- " + strings.Join(st.CommitsDone, "\n- ")
			}
			b.WriteString("\nACTIONS THE PERSON'S OK LET THROUGH IN THIS WORK (a step of the plan's \"each with your OK\" list " +
				"that none of these carried out, at a report, is a point - unless the person said not to; so is one of these " +
				"whose result the commands never read after it ran - its confirmation, or the record's status where it " +
				"shows - the report rests on the result as shown, not on the click): " + done + "\n")
		}
	}
	if len(st.Taught) > 0 {
		b.WriteString("\nWHAT THE PERSON TAUGHT FOR THIS KIND OF WORK (a taught task or an app's guide, as the worker read it; " +
			"the work should follow it):\n" + strings.Join(st.Taught, "\n\n") + "\n")
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
	var unfound, notBacked []string
	for _, item := range unbackedCalls(items, st.judgementSources()) {
		if notBackedRe.MatchString(item) {
			notBacked = append(notBacked, item)
		} else {
			unfound = append(unfound, item)
		}
	}
	if len(unfound) > 0 {
		b.WriteString("\nDECISIONS WHOSE CITED WORDS APPEAR IN NO TEXT THE WORKER WAS SHOWN (pictures it looked at are not " +
			"searched):\n- " + strings.Join(unfound, "\n- ") + "\n")
	}
	if len(notBacked) > 0 && action == "" {
		// Points already open, decided in code (notBackedIssues): the review is told so, not shown them as facts it
		// may let stand.
		b.WriteString("\nALREADY OPEN, DECIDED IN CODE - the worker itself says these rest on nothing given; each goes to " +
			"the person, whatever this review finds, and the work is not done while one stands:\n- " +
			strings.Join(notBacked, "\n- ") + "\n")
	} else if len(notBacked) > 0 {
		b.WriteString("\nDECISIONS THE WORKER SAYS REST ON NOTHING GIVEN:\n- " + strings.Join(notBacked, "\n- ") + "\n")
	}
	if passages := quotedPassages(items, st.judgementSources()); len(passages) > 0 {
		b.WriteString("\nWHERE THE WORDS ITS DECISIONS CITE APPEAR IN WHAT IT WAS SHOWN (the passage around each, from the full outputs):\n- " +
			strings.Join(passages, "\n- ") + "\n")
	}
	if action != "" {
		b.WriteString("\nWHAT IT IS ABOUT TO DO: " + truncateText(action, 600) + "\nThe worker has stopped just before this " +
			"action to ask the person's approval, as it must: it is not done yet, by design. Judge whether the work so far " +
			"and this action are right; do not raise that the action is not done, nor that the parts of the request that " +
			"come after it are not done yet (they are done after it).\n")
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
// evidence, since the ledger keeps only the start of each output. Found live (2026-10-22): the figures
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
	facts := llm.Message{Role: "user", Content: st.reviewFacts(said, items, action)}
	// The pictures the worker last looked at (the screen, a scanned page) go with the facts: figures it read off a
	// screenshot are in no text (found live, 2026-10-22: balances read off the screen were questioned as unshown).
	facts.Images, facts.ImageLabels = st.lastPictures(maxReviewPictures)
	if len(facts.Images) > 0 {
		facts.Content += "\nTHE PICTURES THE WORKER LOOKED AT LAST are attached (what it read off them is shown there).\n"
	}
	for try := 1; try <= 2; try++ {
		resp, err := rc.Provider.Chat(rc.Ctx, rc.Model, []llm.Message{{Role: "system", Content: reviewSystem}, facts},
			llm.Options{TimeoutMs: reviewTimeoutMs, Temperature: reviewTemperature, Purpose: llm.PurposeReview})
		if err != nil {
			rc.Logf("operator", "could not review the work (try %d): %v", try, err)
			continue
		}
		if resp.Truncated {
			// A cut-off answer is not the review: the points past the cut are missing.
			rc.Logf("operator", "the review's answer was cut off (try %d)", try)
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
