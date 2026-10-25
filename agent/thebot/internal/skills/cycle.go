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

const (
	// maxCyclePeriod is the longest round cycleOf finds, and cycleRounds how often it must have gone round.
	maxCyclePeriod = 3
	cycleRounds    = 4
	// maxCycleNudges is how often a round is shown whole before the person is asked.
	maxCycleNudges = 1
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
// output: "" unless the last commands have gone round one short round - two or three commands, each printing what it
// printed the round before - cycleRounds times. Then the round's outputs are shown together, whole, once; when it goes
// round again after that, CycleAsk holds the question the loop puts to the person.
func (st *operatorState) noteRun(command, shown string) string {
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte(elementRefRe.ReplaceAllString(shown, "e#"))))
	st.RecentRuns = append(st.RecentRuns, ranCommand{Command: command, Sum: sum, Shown: shown})
	if n := maxCyclePeriod * cycleRounds; len(st.RecentRuns) > n {
		st.RecentRuns = st.RecentRuns[len(st.RecentRuns)-n:]
	}
	for i := 0; i < len(st.RecentRuns)-maxCyclePeriod; i++ {
		st.RecentRuns[i].Shown = ""
	}
	period := cycleOf(st.RecentRuns)
	if period == 0 {
		return ""
	}
	round := append([]ranCommand(nil), st.RecentRuns[len(st.RecentRuns)-period:]...)
	st.RecentRuns = nil
	commands := make([]string, len(round))
	for i, r := range round {
		commands[i] = "$ " + r.Command
	}
	if st.CycleNudges >= maxCycleNudges {
		st.CycleAsk = cycleQuestion(commands, st.LastIssues)
		return ""
	}
	st.CycleNudges++
	var b strings.Builder
	fmt.Fprintf(&b, "\n\nYou have now run these %d commands in turn %d times over, each printing what it printed the round before:\n%s\n",
		period, cycleRounds, strings.Join(commands, "\n"))
	b.WriteString("Another round shows nothing new. A newer listing of the screen shortens the older ones in this " +
		"conversation, so here is what the others printed last, whole, beside the output above:\n")
	for _, r := range round[:period-1] {
		fmt.Fprintf(&b, "\n$ %s\n%s\n", r.Command, r.Shown)
	}
	b.WriteString("\nSettle what you were checking from these together - write down what each shows that you need. If " +
		"they show the work right, go on to the next point or reply DONE; if something is wrong, correct it; if you " +
		"cannot tell from them, reply HELP with the question for the person. Do not go round them again.")
	return b.String()
}

// cycleOf is the length of the round - two or three commands, not all one - that the last runs have gone round
// cycleRounds times in a row, each command printing what it printed the round before; 0 when there is none.
func cycleOf(runs []ranCommand) int {
	for p := 2; p <= maxCyclePeriod; p++ {
		n := p * cycleRounds
		if len(runs) < n {
			continue
		}
		tail := runs[len(runs)-n:]
		same := true
		for i := p; i < n && same; i++ {
			same = tail[i].Command == tail[i-p].Command && tail[i].Sum == tail[i-p].Sum
		}
		varied := false
		for i := 1; i < p; i++ {
			varied = varied || tail[i].Command != tail[0].Command
		}
		if same && varied {
			return p
		}
	}
	return 0
}

// cycleQuestion is what the person is asked when the worker went round the same commands again after seeing their
// outputs together: the commands, and the points it was settling when there were any.
func cycleQuestion(commands, issues []string) string {
	q := "I keep going back and forth between the same screens and cannot settle what I was checking from them:\n" +
		strings.Join(commands, "\n")
	if len(issues) > 0 {
		q += "\n\nThe points I was settling:\n- " + strings.Join(issues, "\n- ")
	}
	return q + "\n\nTell me what to do about it, or reply \"as you did\" to keep the work as it is."
}
