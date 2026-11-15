package skills

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
)

// Commands taken in turn, round after round. Found live (2026-10-26): sent back to settle a review's points, a
// worker opened two saved records in turn, 45 times over ten minutes. Each new listing of the screen shortens the ones
// before it (screen_digest.go), so the record opened first was out of view once the second was listed - and the guard
// against one command repeated (repeatedCommandNudge) never fired, as no command followed itself.

// Since 2026-11-12 (hermes-agent's agent/tool_guardrails.py, its identical-cycle detector): rounds of two to four
// commands; the model is warned once, with the round's outputs whole, when a round has come round twice more
// (cycleWarnLaps), and the goal stops with a question to the person at the third repeat (cycleStopLaps). A step that
// writes something (changesState, guards.go) starts the count again.

const (
	// maxCyclePeriod is the longest round cycleOf finds.
	maxCyclePeriod = 4
	// cycleWarnLaps is how many times in a row a round must have gone round for the warning, cycleStopLaps for the stop.
	cycleWarnLaps = 3
	cycleStopLaps = 4
)

// elementRefRe is an element's ref in a listing (e1107): new at each listing of the same screen.
var elementRefRe = regexp.MustCompile(`\be\d+\b`)

// ranCommand is one command run, a sum of what it printed (refs left out), and - for the last few - the text shown.
type ranCommand struct {
	Command string
	Sum     string
	Shown   string `json:",omitempty"`
}

// noteRun keeps a command just run with what it printed (shown), and returns what the model is told beside that
// output: "" unless the last commands have gone round one short round - two to four commands, each printing what it
// printed the round before. At cycleWarnLaps rounds the round's outputs are shown together, whole, once; at
// cycleStopLaps CycleAsk holds the question the loop puts to the person (or, asked once already this goal, their
// answer is given back). A command that writes something (changesState) starts the count again.
func (st *operatorState) noteRun(command, shown string) string {
	if changesState(command) {
		st.RecentRuns = nil
		return ""
	}
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte(elementRefRe.ReplaceAllString(shown, "e#"))))
	st.RecentRuns = append(st.RecentRuns, ranCommand{Command: command, Sum: sum, Shown: shown})
	if n := maxCyclePeriod * cycleStopLaps; len(st.RecentRuns) > n {
		st.RecentRuns = st.RecentRuns[len(st.RecentRuns)-n:]
	}
	for i := 0; i < len(st.RecentRuns)-maxCyclePeriod; i++ {
		st.RecentRuns[i].Shown = ""
	}
	period, covered := cycleOf(st.RecentRuns)
	if period == 0 {
		return ""
	}
	laps := covered / period
	round := append([]ranCommand(nil), st.RecentRuns[len(st.RecentRuns)-period:]...)
	commands := make([]string, len(round))
	for i, r := range round {
		commands[i] = "$ " + r.Command
	}
	if laps >= cycleStopLaps {
		st.RecentRuns = nil
		if st.CycleAsked {
			// Asked once a goal: found live (2026-10-27), the same "going back and forth" question went to the person
			// twice in four minutes, their first answer unused. Their answer is given back, and the loop is pushed on.
			return cycleAfterAnswer(commands, st.PersonSaid)
		}
		st.CycleAsked = true
		st.CycleAsk = cycleQuestion(round, laps, st.LastIssues)
		return ""
	}
	if covered != period*cycleWarnLaps {
		return ""
	}
	st.CycleNudges++
	var b strings.Builder
	fmt.Fprintf(&b, "\n\nYou have now run these %d commands in turn %d times over, each printing what it printed the round before:\n%s\n",
		period, laps, strings.Join(commands, "\n"))
	b.WriteString("Another round shows nothing new. A newer listing of the screen shortens the older ones in this " +
		"conversation, so here is what the others printed last, whole, beside the output above:\n")
	for _, r := range round[:period-1] {
		fmt.Fprintf(&b, "\n$ %s\n%s\n", r.Command, r.Shown)
	}
	b.WriteString("\nSettle what you were checking from these together - write down what each shows that you need. If " +
		"they show the work right, go on to the next point or reply DONE; if something is wrong, correct it; if you " +
		"cannot tell from them, reply HELP with the question for the person. Do not go round them again: one more " +
		"round stops the task and puts the question to the person.")
	return b.String()
}

// cycleOf is the length of the shortest round - two to four commands, not all one - the last runs have gone round at
// least cycleWarnLaps times, each command (spacing aside) printing what it printed the round before, and how many of
// the last runs that repetition covers (whatever command the round is counted from, so the count only grows while the
// round goes on: the warning comes once); 0, 0 when there is none.
func cycleOf(runs []ranCommand) (period, covered int) {
	same := func(a, b ranCommand) bool {
		return a.Sum == b.Sum && canonicalCommand(a.Command) == canonicalCommand(b.Command)
	}
	for p := 2; p <= maxCyclePeriod; p++ {
		if len(runs) < p*cycleWarnLaps {
			continue
		}
		last := runs[len(runs)-p:]
		varied := false
		for i := 1; i < p; i++ {
			varied = varied || canonicalCommand(last[i].Command) != canonicalCommand(last[0].Command)
		}
		if !varied {
			continue
		}
		n := p
		for i := len(runs) - 1; i-p >= 0 && same(runs[i], runs[i-p]); i-- {
			n++
		}
		if n >= p*cycleWarnLaps {
			return p, n
		}
	}
	return 0, 0
}

// cycleAfterAnswer is what the model is told when it goes round the same commands again after the person was asked
// once: their answer, and that it is not asked again.
func cycleAfterAnswer(commands []string, said []string) string {
	answer := ""
	if n := len(said); n > 0 {
		answer = truncateText(said[n-1], 500)
	}
	var b strings.Builder
	b.WriteString("\n\nYou have gone round these same screens again:\n" + strings.Join(commands, "\n") + "\n")
	if answer != "" {
		b.WriteString("The person was asked about this once and answered: " + answer + "\n")
	}
	b.WriteString("They are not asked again. Do what they said: write down, from the outputs above, each value you need " +
		"from these screens, then leave them and go on to the next step of the task. If something on them is wrong, " +
		"correct it where it is; if you cannot, say so in your report as NOT done.")
	return b.String()
}

// cycleQuestion is what the person is asked when the worker went round the same commands cycleStopLaps times: the
// commands with the first line each printed, that nothing more was checked, and the points it was settling when there
// were any.
func cycleQuestion(round []ranCommand, laps int, issues []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "I kept going back and forth between the same %d commands - %d rounds, each command printing exactly "+
		"what it printed the round before - and could not settle what I was checking from them, so I have stopped. "+
		"Nothing after this was checked:\n", len(round), laps)
	for _, r := range round {
		b.WriteString("$ " + r.Command + "\n")
		if line := firstPrintedLine(r.Shown); line != "" {
			b.WriteString("  printed: " + line + "\n")
		}
	}
	if len(issues) > 0 {
		b.WriteString("\nThe points I was settling:\n- " + strings.Join(issues, "\n- ") + "\n")
	}
	b.WriteString("\nTell me what to do about it, or reply \"as you did\" to keep the work as it is.")
	return b.String()
}

// firstPrintedLine is the first line of output a formatted command result shows, cut to 200 bytes; "" when none.
func firstPrintedLine(shown string) string {
	const mark = "\noutput:\n"
	if i := strings.Index(shown, mark); i >= 0 {
		shown = shown[i+len(mark):]
	}
	for _, line := range strings.Split(shown, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return truncateText(line, 200)
		}
	}
	return ""
}
