package skills

import (
	"fmt"
	"strings"

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
// Nothing the checks rely on is lost: grounding traces against st.Seen, which keeps every output every command showed.
// A local model is left as it was (its context and its prefix cache are its own).
// ============================================================

const (
	// contextBudgetChars is how long the re-sent transcript (characters, a picture counted as pictureChars) may grow
	// before its older part is compacted.
	contextBudgetChars = 200_000
	pictureChars       = 4_000
	// keepRecentMessages is how many of the newest messages a compaction keeps word for word.
	keepRecentMessages = 10
	// maxContinuations bounds how many more turn budgets a goal that keeps making progress gets.
	maxContinuations = 4
	// minWindowProgress is how many commands must have worked in a budget for it to count as progress.
	minWindowProgress = 5
	maxLedger         = 400
	ledgerShown       = 90
	maxNotesChars     = 8_000
)

// compactNoteRequest asks the model for its notes before its older context is replaced by them.
const compactNoteRequest = "\n\n(Before you go on: your working context is about to be shortened to save room. The older " +
	"part of this conversation will be replaced by notes; the request, your instructions and the newest steps stay. " +
	"Write the notes you need to carry on without the older part, after NOTES: - (1) what is done, and what showed it " +
	"(the screen, a file, a message); (2) every value you are using that you read earlier, each with where it came from " +
	"(file and cell or line, document, screen field); (3) what remains, in order; (4) open doubts, and what the person " +
	"said. Do not run a command in this reply.)"

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

// transcriptChars is the size of what is re-sent each turn.
func transcriptChars(st *operatorState) int {
	n := 0
	for _, m := range st.Messages {
		n += len(m.Content) + pictureChars*len(m.Images)
	}
	return n
}

// noteLedger records what a command did, for a compaction's progress note.
func (st *operatorState) noteLedger(command string, res shellResult) {
	line := fmt.Sprintf("$ %s -> %s", truncateText(strings.Join(strings.Fields(command), " "), 300), ledgerOutput(res))
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
	if resp, err := rc.Provider.Chat(rc.Ctx, rc.Model, ask, llm.Options{TimeoutMs: 180000, Temperature: 0.2}); err == nil {
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
		shown := st.Ledger
		if len(shown) > ledgerShown {
			fmt.Fprintf(&b, "\nWhat the commands did, as recorded (the last %d of %d, latest last):\n", ledgerShown, len(shown))
			shown = shown[len(shown)-ledgerShown:]
		} else {
			b.WriteString("\nWhat the commands did, as recorded (latest last):\n")
		}
		b.WriteString(strings.Join(shown, "\n") + "\n")
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
	b.WriteString("\nEverything a command showed earlier is still known to the checks. Read a file or a range again " +
		"(office read) or look at the screen (gui observe) whenever you need exact words or figures - never type a value " +
		"from memory. Carry on from where the notes leave off, and never redo a step the record shows done.")

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
	rc.Logf("operator", "context shortened (%s): %d -> %d characters re-sent, %d newest messages kept word for word", why,
		before, transcriptChars(st), len(st.Messages)-prefix-1)
}
