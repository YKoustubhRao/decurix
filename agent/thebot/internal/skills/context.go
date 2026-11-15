package skills

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"thebot/internal/llm"
)

// ============================================================
// A long goal's context, kept to what it needs
//
// Found live (2026-10-22): a return entered by hand took 412 steps; every turn re-sent the whole transcript
// (5-9M tokens a task), and at 80 turns the goal was told "you have 8 turns left - do not begin anything new, finish"
// and it did: it generated the summary with a mismatch warning on it and reported. A long goal is not a failing one.
//
// So, for a hosted model:
//   - when the transcript re-sent each turn grows past contextBudgetChars, its older part is replaced by a progress
//     note: the model's own notes (written just before, with the whole context still in front of it), the harness's
//     record of what every command did, and every word the person said in this goal, verbatim. The request and the
//     contract (the prefix) and the newest steps stay as they were;
//   - when the turn budget is spent and the window made progress, the goal is compacted the same way and given another
//     budget, up to maxContinuations times, instead of being told to wrap up or stopping to ask.
//
// Nothing the checks rely on is lost: grounding traces against st.Seen, which keeps what every command showed. Nor is
// it lost to the model: an output the record shortens is kept in a file the record names, so a step can be read again
// even when its screen has since changed.
//
// What a compaction keeps is bounded in characters, not only in messages. Found in the logs (2026-10-30): with 10 kept
// messages of up to 16,000 characters and 90 ledger lines of up to 1,000, a long goal's "shortened" transcript came to
// 197,000 of its 200,000, and it compacted again 37 seconds later, a whole-transcript model call each time.
//
// A local model keeps its transcript as it is while it fits: its prefix cache is what makes a turn take seconds, not
// minutes, on the CPU host. But it has a fixed window (num_ctx 8192), and past it Ollama silently drops the oldest
// messages after the system one - the contract and the task first (review T-09). So before each local call the
// transcript is measured: over the window, old command outputs are masked (the prefix, message 1 included, never is);
// still over, the goal ends as not checked. Under the window nothing changes, and the prefix stays byte-identical.
// ============================================================

const (
	// contextBudgetChars is how long the re-sent transcript (characters, a picture counted as pictureChars) may grow
	// before its older part is compacted.
	contextBudgetChars = 200_000
	pictureChars       = 4_000
	// keepRecentMessages and keepRecentChars bound the newest messages a compaction keeps word for word; at least
	// minRecentMessages stay whatever their size.
	keepRecentMessages = 10
	keepRecentChars    = 40_000
	minRecentMessages  = 4
	// compactRegrowth is how much a transcript must grow after a compaction before another is worth its model call.
	compactRegrowth = contextBudgetChars / 4
	// maxContinuations bounds how many more turn budgets a goal that keeps making progress gets.
	maxContinuations = 4
	// minWindowProgress is how many commands must have worked in a budget for it to count as progress.
	minWindowProgress = 5
	maxLedger         = 400
	ledgerShown       = 90
	ledgerShownChars  = 30_000
	recordsShownChars = 20_000
	maxNotesChars     = 8_000
)

// The local window (llm/ollama.go sends num_ctx 8192 on every call) and what is kept free for the answer. Tokens are
// estimated from characters at localCharsPerToken - fewer characters per token than prose usually has, so the estimate
// runs high: the margin that keeps a request under the window when its text tokenises densely (figures, code).
const (
	localContextTokens = 8192
	localAnswerReserve = 1024
	localCharsPerToken = 3
	localMessageTokens = 8 // the chat template's own tokens around each message
	localBudgetTokens  = localContextTokens - localAnswerReserve
	// localMaskKeepNewest is how many newest messages a local masking keeps whole: the last command and its output.
	// The hosted pass keeps maskKeepNewest; a local window holds about five full outputs beside the prefix.
	localMaskKeepNewest = 2
	outOfLocalRoomWhy   = "out of room for this task on the local model"
	outOfLocalRoomWords = "I stopped: this task no longer fits in the local model's working memory, even with older " +
		"command outputs shortened."
)

// localTokens is the estimated size of messages in tokens on the local model (a picture counted as pictureChars), at
// the default rate - before the model has said what a call really cost.
func localTokens(messages []llm.Message) int {
	return estimateTokens(messages, 0)
}

// estimateTokens is the size of messages at perByte tokens a byte, never fewer than the default rate gives.
func estimateTokens(messages []llm.Message, perByte float64) int {
	bytes := messagesChars(messages)
	n := (bytes + localCharsPerToken - 1) / localCharsPerToken
	if measured := int(float64(bytes)*perByte + 0.999); measured > n {
		n = measured
	}
	return n + localMessageTokens*len(messages)
}

// noteLocalCost keeps the highest tokens-a-byte rate a local call has really cost (Ollama's prompt_eval_count over the
// bytes sent). Text in a non-Latin script costs far more tokens a byte than the default assumes (review N-W3-6); a
// call answered partly from the prompt cache reports fewer tokens than it holds, so a rate only ever goes up.
func (st *operatorState) noteLocalCost(sent []llm.Message, promptTokens int) {
	bytes := messagesChars(sent)
	if promptTokens <= 0 || bytes == 0 {
		return
	}
	if rate := float64(promptTokens) / float64(bytes); rate > st.LocalTokensPerByte {
		st.LocalTokensPerByte = rate
	}
}

// localEstimate is the transcript's size in tokens on the local model, at the rate its calls have shown.
func (st *operatorState) localEstimate() int {
	return estimateTokens(st.Messages, st.LocalTokensPerByte)
}

// localOverBudget is whether the transcript would not fit the local window with room for the answer.
func (st *operatorState) localOverBudget() bool {
	return st.localEstimate() > localBudgetTokens
}

// compactNoteRequest asks the model for its notes before its older context is replaced by them.
const compactNoteRequest = "\n\n(Before you go on: your working context is about to be shortened to save room. The older " +
	"part of this conversation will be replaced by notes; the request, your instructions and the newest steps stay. " +
	"Write the notes you need to carry on without the older part, after NOTES: - (1) what is done, and what showed it " +
	"(the screen, a file, a message); (2) every value you are using that you read earlier, each with where it came from " +
	"(file and cell or line, document, screen field); (3) what remains, in order; (4) open doubts, and what the person " +
	"said. Anything you believe but did not see confirmed (by a command's output, the screen, a file or the person) goes " +
	"on a line of its own as UNVERIFIED: the claim - what it rests on; never write it as a fact. Do not run a command " +
	"in this reply.)"

// prefixEnd is where the goal's fixed part ends: the system frame, any session history and the contract with the
// request. A checkpoint written before PrefixLen existed finds it: the first user message that holds the request.
func (st *operatorState) prefixEnd() int {
	if st.PrefixLen > 0 && st.PrefixLen <= len(st.Messages) {
		return st.PrefixLen
	}
	for i, m := range st.Messages {
		if m.Role == "user" && st.Request != "" && strings.Contains(m.Content, st.Request) {
			return i + 1
		}
	}
	return min(1, len(st.Messages))
}

// messagesChars is the size of messages, a picture counted at pictureCharsNow.
func messagesChars(messages []llm.Message) int {
	n, picture := 0, pictureCharsNow()
	for _, m := range messages {
		n += len(m.Content) + picture*len(m.Images)
	}
	return n
}

// learnedPictureTokens is what a picture has really cost on the brain's route, learned from the provider's usage
// (llm/imagecost.go); a variable so tests can set it.
var learnedPictureTokens = llm.ActiveImageTokens

// pictureCharsNow is what a picture counts for in the budgets: its learned price in tokens at localCharsPerToken,
// never less than pictureChars. A floor, not a replacement: a picture priced low would let a local transcript past
// the 8192-token window, where Ollama drops the oldest messages without a word; one priced high (a local vision
// encoder's four thousand tokens) is what learning it is for.
func pictureCharsNow() int {
	if tokens, ok := learnedPictureTokens(); ok {
		return max(pictureChars, tokens*localCharsPerToken)
	}
	return pictureChars
}

// transcriptChars is the size of what is re-sent each turn.
func transcriptChars(st *operatorState) int {
	return messagesChars(st.Messages)
}

// noteLedger records what a command did, for a compaction's progress note.
func (st *operatorState) noteLedger(command string, res shellResult) {
	line := fmt.Sprintf("$ %s -> %s", truncateText(strings.Join(strings.Fields(command), " "), 300), ledgerOutput(res))
	if ledgerShortens(res) {
		if path := st.keepOutput(res.Output); path != "" {
			line += " (full output: " + path + ")"
		}
	}
	st.Ledger = append(st.Ledger, line)
	if len(st.Ledger) > maxLedger {
		st.Ledger = st.Ledger[len(st.Ledger)-maxLedger:]
	}
}

// maxLedgerLines and maxLedgerOutput bound an output the ledger keeps whole (ledgerOutput).
const (
	maxLedgerLines  = 15
	maxLedgerOutput = 700
)

// ledgerOutput is what the ledger keeps of a command's output: a short one whole (a listing of files, a few lines of
// results), else its first line and how many more there were - never the first line alone as if it were all. Found
// live (2026-10-25): a find that listed six bills was shown to the review by its first line, and the review held that
// the folder had only that one, twice.
func ledgerOutput(res shellResult) string {
	out := strings.TrimSpace(res.Output)
	lines := strings.Split(out, "\n")
	if out == "" || len(lines) == 1 {
		return truncateText(headline(res), 220)
	}
	if len(lines) <= maxLedgerLines && len(out) <= maxLedgerOutput {
		for i := range lines {
			lines[i] = strings.TrimSpace(lines[i])
		}
		return fmt.Sprintf("exit %d · %s", res.ExitCode, strings.Join(lines, " | "))
	}
	return fmt.Sprintf("%s (+%d more lines)", truncateText(headline(res), 220), len(lines)-1)
}

// ledgerShortens reports whether the ledger keeps less of this output than it was (ledgerOutput).
func ledgerShortens(res shellResult) bool {
	out := strings.TrimSpace(res.Output)
	lines := strings.Split(out, "\n")
	if len(lines) == 1 {
		return len(out) > 220
	}
	return len(lines) > maxLedgerLines || len(out) > maxLedgerOutput
}

// declaredRecords is what `gui records` lists, "" when there are none. Notes cannot be relied on to say whether item X
// of N is done (a bounded summary cannot answer that in general); the harness holds the answer, so it carries it over.
var declaredRecords = func(rc RunContext, st *operatorState) string {
	res := runShell(rc.Ctx, rc.Dir(), "gui records", 30*time.Second, st.guiEnv()...)
	out := strings.TrimSpace(res.Output)
	if res.ExitCode != 0 || out == "" || strings.HasPrefix(strings.ToLower(out), "no record") {
		return ""
	}
	return out
}

// shownLedger is the newest of the record that a progress note has room for, in order.
func shownLedger(ledger []string) []string {
	first, room := len(ledger), ledgerShownChars
	for first > 0 && len(ledger)-first < ledgerShown && room >= len(ledger[first-1]) {
		first--
		room -= len(ledger[first])
	}
	return ledger[first:]
}

// Older command outputs are masked well before a compaction is due: each one older than the newest few keeps its start
// and says where its whole text is (a file the goal keeps), with no model call. Measured (2026-10-31): a task made
// ~200 calls of ~36k prompt tokens each, the transcript swinging between a compaction's ~90k characters and the 200k
// budget; old outputs are most of it, and masking them matched model summaries at about half the cost elsewhere
// (JetBrains, "The Complexity Trap", 2025). It is done in a maintenance pass, never turn by turn: rewriting earlier
// messages breaks the model provider's prompt cache from that message on.
const (
	maskKeepNewest = 10    // the newest messages are never masked
	maskMinChars   = 1_500 // a shorter output costs less than its stub
	maskHeadChars  = 600   // what a masked output keeps of its start
	maskMarker     = "\n[... masked: "
)

// A hosted goal's transcript is only appended to between maintenance passes. Measured (2026-10-31, gpt-6.1-sol): a
// call whose previous message had been changed (its screenshot taken off, an earlier listing digested) was served
// 12,160 cached tokens - the frame alone - where one with nothing earlier changed was served 38-41k: on these models
// any rewrite of the transcript's tail costs the whole cache. So stale screen listings, older screenshots and older
// outputs wait, and one pass deals with all of them once they are worth the break.
//
// Worth the break is a matter of share, not count. Cached input is billed at about a tenth of the price: a pass
// re-sends the C-r characters it keeps at the full price once, and saves a tenth of r on every later turn, so it pays
// back after about 9(C-r)/r turns - 13 turns when it takes off 40% of the transcript, 81 when it takes off 10%, more
// than most goals have left. Measured (2026-11-01, probe of a 64-minute task): passes due by counts (four older
// screenshots, 20k of stale listings) came every 3-4 turns and were 64% of the task's uncached input; what the task
// kept adding was 22%.
const (
	maintainShare    = 0.4    // the share of the transcript a pass must take off to be worth the cache it costs
	maintainMinChars = 40_000 // a shorter transcript is cheap to re-send either way
	staleDigestChars = 600    // about what a digested screen listing keeps (digestScreen)
)

// staleScreens are the screen outputs a pass would digest: every one tracked but the newest, whose refs are in use.
func (st *operatorState) staleScreens() []int {
	if len(st.ScreenMsgs) <= 1 {
		return nil
	}
	return st.ScreenMsgs[:len(st.ScreenMsgs)-1]
}

// oldScreenshots counts the screenshots older than the newest one still shown.
func (st *operatorState) oldScreenshots() int {
	n := 0
	for _, m := range st.Messages {
		if len(m.Images) > 0 && strings.HasSuffix(m.Content, screenshotNote) {
			n++
		}
	}
	return max(0, n-1)
}

// removableChars is what a pass would take off the transcript: stale screen listings down to their digests,
// screenshots older than the newest, and older outputs down to their stubs.
func (st *operatorState) removableChars() int {
	n, stale := 0, map[int]bool{}
	for _, idx := range st.staleScreens() {
		if idx >= 0 && idx < len(st.Messages) {
			stale[idx] = true
			n += max(0, len(st.Messages[idx].Content)-staleDigestChars)
		}
	}
	n += st.oldScreenshots() * pictureCharsNow()
	for i := st.prefixEnd(); i < len(st.Messages)-maskKeepNewest; i++ {
		if !stale[i] && maskable(st.Messages[i]) {
			n += len(st.Messages[i].Content) - maskHeadChars
		}
	}
	return n
}

// maintenanceDue is whether what has piled up is worth one break of the prompt cache: a large enough share of a
// transcript large enough to matter (maintainShare, maintainMinChars).
func (st *operatorState) maintenanceDue() bool {
	n := transcriptChars(st)
	return n >= maintainMinChars && float64(st.removableChars()) >= maintainShare*float64(n)
}

// maintain does, in one pass, every rewrite the transcript waits for: stale listings digested (screen_digest.go),
// screenshots older than the newest taken off, and older outputs masked. The cache is broken once either way, so
// nothing removable is left for a later pass. Says what it did.
func (st *operatorState) maintain() string {
	var did []string
	if stale := st.staleScreens(); len(stale) > 0 {
		newest := st.ScreenMsgs[len(st.ScreenMsgs)-1]
		st.ScreenMsgs = stale
		st.digestStaleScreens()
		st.ScreenMsgs = []int{newest}
		did = append(did, fmt.Sprintf("%d stale screens digested", len(stale)))
	}
	if old := st.oldScreenshots(); old > 0 {
		st.forgetOldPictures(screenshotNote, 1)
		did = append(did, fmt.Sprintf("%d older screenshots taken off", old))
	}
	if masked := st.maskOldOutputs(); masked > 0 {
		did = append(did, fmt.Sprintf("%d older outputs masked", masked))
	}
	return strings.Join(did, ", ")
}

// maskable is a command output a pass would mask: a user message of plain text, long enough, not yet masked. The
// request, the person's words, notes and pictures are never touched.
func maskable(m llm.Message) bool {
	return m.Role == "user" && len(m.Images) == 0 && len(m.Content) >= maskMinChars &&
		strings.HasPrefix(m.Content, "Command execution result") && !strings.Contains(m.Content, maskMarker)
}

// maskOldOutputs masks, in one pass, every command output older than the newest maskKeepNewest messages and longer
// than maskMinChars: its first maskHeadChars stay, the rest is in a file it names. An output whose file cannot be
// written stays whole. The checks are unaffected (st.Seen keeps what every command showed). Returns how many it masked.
func (st *operatorState) maskOldOutputs() int {
	return st.maskOutputsBefore(maskKeepNewest)
}

// maskOutputsBefore is maskOldOutputs keeping the newest keep messages whole.
func (st *operatorState) maskOutputsBefore(keep int) int {
	masked := 0
	for i := st.prefixEnd(); i < len(st.Messages)-keep; i++ {
		m := &st.Messages[i]
		if !maskable(*m) {
			continue
		}
		path := st.keepOutput(m.Content)
		if path == "" {
			continue
		}
		cut := maskHeadChars
		for cut > 0 && !utf8.RuneStart(m.Content[cut]) {
			cut--
		}
		m.Content = m.Content[:cut] + fmt.Sprintf("%s%d more characters; the whole output is in %s - cat it to read it again]",
			maskMarker, len(m.Content)-cut, path)
		masked++
	}
	// (a masked screen is not digested again later)
	kept := st.ScreenMsgs[:0]
	for _, idx := range st.ScreenMsgs {
		if idx >= 0 && idx < len(st.Messages) && !strings.Contains(st.Messages[idx].Content, maskMarker) {
			kept = append(kept, idx)
		}
	}
	st.ScreenMsgs = kept
	st.MaskedTo = transcriptChars(st)
	return masked
}

// contextOverBudget is whether a compaction is due: past the budget, and grown enough since the last one to be worth
// its model call. A goal whose fixed part alone fills the room would otherwise compact on every turn.
func (st *operatorState) contextOverBudget() bool {
	n := transcriptChars(st)
	return n > contextBudgetChars && (st.CompactedTo == 0 || n >= st.CompactedTo+compactRegrowth)
}

// personSaid is every exchange with the person in messages: the question the model asked (HELP) and what came back,
// and every reply, approval refusal or confirmation the harness handed on - word for word.
func personSaid(messages []llm.Message) []string {
	var out []string
	for i, m := range messages {
		if m.Role == "user" && (strings.HasPrefix(m.Content, "The human ") || strings.HasPrefix(m.Content, "The person ")) {
			asked := ""
			if i > 0 && messages[i-1].Role == "assistant" {
				if action, value := parseControl(messages[i-1].Content); action == "help" {
					asked = "You asked: " + truncateText(value, 600) + "\n  "
				}
			}
			out = append(out, asked+truncateText(m.Content, 2000))
		}
	}
	return out
}

// mayContinue is whether a goal whose turn budget is spent gets another one: hosted, budgets left, and the window just
// spent made progress.
func (st *operatorState) mayContinue(windowStart int) bool {
	return st.Hosted && st.Continuations < maxContinuations && st.CommandsSucceeded-windowStart >= minWindowProgress
}

// compact replaces the older part of the transcript by a progress note (see above). why is for the log.
func (o *OperatorSkill) compact(rc RunContext, st *operatorState, why string) {
	prefix := st.prefixEnd()
	if len(st.Messages)-prefix <= keepRecentMessages+2 {
		return
	}
	cut := len(st.Messages) - keepRecentMessages
	for kept := messagesChars(st.Messages[cut:]); kept > keepRecentChars && len(st.Messages)-cut > minRecentMessages; cut++ {
		kept -= messagesChars(st.Messages[cut : cut+1])
	}
	// The kept part starts with the model's own reply, so the progress note (a user message) comes before it.
	for cut > prefix && st.Messages[cut].Role != "assistant" {
		cut--
	}
	if cut <= prefix+1 {
		return
	}

	notes := ""
	ask := append([]llm.Message(nil), st.Messages...)
	last := len(ask) - 1
	if ask[last].Role == "user" {
		ask[last] = llm.Message{Role: "user", Content: ask[last].Content + compactNoteRequest}
	} else {
		ask = append(ask, llm.Message{Role: "user", Content: strings.TrimSpace(compactNoteRequest)})
	}
	if resp, err := rc.Provider.Chat(rc.Ctx, rc.Model, ask, llm.Options{TimeoutMs: 180000, Temperature: 0.2, Purpose: llm.PurposeCompact}); err == nil {
		notes = strings.TrimSpace(resp.Text)
		if i := strings.Index(strings.ToUpper(notes), "NOTES:"); i >= 0 {
			notes = strings.TrimSpace(notes[i+len("NOTES:"):])
		}
		notes = truncateText(notes, maxNotesChars)
	} else {
		rc.Logf("operator", "could not get notes before shortening the context: %v", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "PROGRESS SO FAR - the older part of this goal (%d steps) was shortened to save room.\n", st.CommandsRun)
	if notes != "" {
		b.WriteString("\nYour own notes, written just before:\n" + notes + "\n")
	}
	if len(st.Ledger) > 0 {
		shown := shownLedger(st.Ledger)
		if len(shown) < len(st.Ledger) {
			fmt.Fprintf(&b, "\nWhat the commands did, as recorded (the last %d of %d, latest last):\n", len(shown), len(st.Ledger))
		} else {
			b.WriteString("\nWhat the commands did, as recorded (latest last):\n")
		}
		b.WriteString(strings.Join(shown, "\n") + "\n")
	}
	if records := declaredRecords(rc, st); records != "" {
		b.WriteString("\nThe records declared in this goal, with what was read back (the harness's own, not your notes; " +
			"this is what says which items are done):\n" + truncateText(records, recordsShownChars) + "\n")
	}
	// Everything the person said in the goal, kept apart (resume), so a second compaction does not lose what the first
	// folded into its note; a goal checkpointed before that was kept reads it from the part being folded.
	said := st.PersonSaid
	if len(said) == 0 {
		said = personSaid(st.Messages[prefix:cut])
	}
	if len(said) > 0 {
		b.WriteString("\nWhat the person said in this goal, word for word (it still holds):\n- " + strings.Join(said, "\n- ") + "\n")
	}
	if anchors := exactIdentifiers(st.Messages[prefix:cut], st.Messages[:prefix]); len(anchors) > 0 {
		b.WriteString("\nExact strings from the shortened part, most frequent first (taken from it mechanically: a pointer for " +
			"finding a value again, not a check that it is right):\n" + strings.Join(anchors, "  ") + "\n")
	}
	b.WriteString("\nEverything a command showed earlier is still known to the checks. A step marked (full output: PATH) " +
		"can be read again with cat PATH, even if that screen has since changed. For what stands now, read the file or " +
		"range again (office read) or look at the screen (gui observe) whenever you need exact words or figures - never " +
		"type a value from memory. Carry on from where the notes leave off, and never redo a step the record shows done.")

	kept := make([]llm.Message, 0, prefix+1+len(st.Messages)-cut)
	kept = append(kept, st.Messages[:prefix]...)
	kept = append(kept, llm.Message{Role: "user", Content: b.String()})
	shift := cut - (prefix + 1)
	var screens []int
	for _, idx := range st.ScreenMsgs {
		if idx >= cut {
			screens = append(screens, idx-shift)
		}
	}
	kept = append(kept, st.Messages[cut:]...)
	before := transcriptChars(st)
	st.Messages, st.ScreenMsgs = kept, screens
	st.Compactions++
	st.CompactedTo = transcriptChars(st)
	rc.Logf("operator", "context shortened (%s): %d -> %d characters re-sent, %d newest messages kept word for word", why,
		before, transcriptChars(st), len(st.Messages)-prefix-1)
}

// A compaction's progress note also lists the exact strings the shortened part held: identifiers, figures, dates,
// paths and addresses, ranked by how often they appeared. Notes are a summary, and a summary loses exact strings first;
// a mechanical list keeps them findable (hermes-agent measured, in its own compaction eval, that the person's words verbatim, a pointer
// to recover the rest and such a list carry what a compacted goal recalls). The shapes are generic, never a product's
// or a domain's: numbers of four or more digits or with decimals, dates, file paths, email addresses, URLs, and tokens
// that mix letters and digits.
const (
	maxAnchorChars = 2_000 // what the list may take of the note
	maxAnchorLen   = 200   // a longer string is not an identifier
	minMixedLen    = 4     // a shorter letters-and-digits token is noise ("h1", "2nd")
)

var (
	// anchorWholeRe finds the shapes that are taken whole, before the text is split into tokens: URLs, email
	// addresses, Windows and Unix paths (two parts or more, or from ~), and dates.
	anchorWholeRe = regexp.MustCompile(`https?://[^\s<>"'()\[\]{}]+` +
		`|[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}` +
		`|\b[A-Za-z]:\\[^\s"'<>|*?]+` +
		`|(?:~|\.{1,2})?(?:/[A-Za-z0-9._-]+){2,}/?|~/[A-Za-z0-9._-]+` +
		`|\b\d{4}-\d{2}-\d{2}\b|\b\d{1,2}[/.-]\d{1,2}[/.-]\d{2,4}\b`)
	// anchorTokenRe splits what is left into tokens; anchorNumberRe is a token that is a number.
	anchorTokenRe  = regexp.MustCompile(`[A-Za-z0-9][A-Za-z0-9_.,-]*`)
	anchorNumberRe = regexp.MustCompile(`^\d[\d,]*(?:\.\d+)?$`)
)

// exactIdentifiers is the list of exact strings in region worth carrying past a compaction, most frequent first (ties
// in the order first seen), leaving out what the kept prefix already shows, within maxAnchorChars.
func exactIdentifiers(region, prefix []llm.Message) []string {
	count := map[string]int{}
	var order []string
	note := func(s string) {
		s = strings.TrimRight(s, ".,;:-_/")
		if s == "" || len(s) > maxAnchorLen {
			return
		}
		if count[s] == 0 {
			order = append(order, s)
		}
		count[s]++
	}
	for _, m := range region {
		rest := anchorWholeRe.ReplaceAllStringFunc(m.Content, func(s string) string {
			note(s)
			return " "
		})
		for _, tok := range anchorTokenRe.FindAllString(rest, -1) {
			tok = strings.TrimRight(tok, ".,-_")
			if anchorNumberRe.MatchString(tok) {
				if strings.ContainsAny(tok, ".,") || len(tok) >= 4 {
					note(tok)
				}
				continue
			}
			for _, part := range strings.Split(tok, ",") {
				if part = strings.Trim(part, ".-_"); len(part) >= minMixedLen && mixesLettersAndDigits(part) {
					note(part)
				}
			}
		}
	}
	var fixed strings.Builder
	for _, m := range prefix {
		fixed.WriteString(m.Content)
		fixed.WriteByte('\n')
	}
	shown := fixed.String()
	sort.SliceStable(order, func(i, j int) bool { return count[order[i]] > count[order[j]] })
	var out []string
	room := maxAnchorChars
	for _, s := range order {
		if strings.Contains(shown, s) {
			continue
		}
		if len(s)+2 > room {
			continue
		}
		room -= len(s) + 2
		out = append(out, s)
	}
	return out
}

// mixesLettersAndDigits reports whether s holds both a letter and a digit.
func mixesLettersAndDigits(s string) bool {
	letter, digit := false, false
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digit = true
		case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z':
			letter = true
		}
	}
	return letter && digit
}
