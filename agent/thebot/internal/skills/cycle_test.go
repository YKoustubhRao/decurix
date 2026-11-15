package skills

import (
	"fmt"
	"strings"
	"testing"
)

// The live shape (2026-10-26): two saved records opened in turn, each listing with new refs.
func voucherRun(st *operatorState, id string, round int) string {
	cmd := fmt.Sprintf("gui open 'http://h:8704/books/voucher?id=%s' && gui observe", id)
	out := fmt.Sprintf("Opened voucher %s\ne%d heading \"Sales Voucher No. %s\"\ne%d link \"Alter\"", id, 100*round, id, 100*round+1)
	return st.noteRun(cmd, out)
}

func TestTwoCommandsInTurnAreShownTogetherOnceThenAsked(t *testing.T) {
	st := &operatorState{LastIssues: []string{"voucher 1191's party is not the books' name"}}
	var said []string
	for round := 1; round <= cycleWarnLaps; round++ {
		said = append(said, voucherRun(st, "2006", round), voucherRun(st, "2009", round))
	}
	for _, s := range said[:len(said)-1] {
		if s != "" {
			t.Fatalf("a note before %d rounds: %q", cycleWarnLaps, s)
		}
	}
	last := said[len(said)-1]
	if !strings.Contains(last, "run these 2 commands in turn 3 times") || !strings.Contains(last, "Opened voucher 2006") ||
		!strings.Contains(last, "Do not go round them again") {
		t.Fatalf("the round is not shown whole: %q", last)
	}
	if st.CycleAsk != "" || st.CycleNudges != 1 {
		t.Fatalf("asked already: %q (%d)", st.CycleAsk, st.CycleNudges)
	}
	// One round more (the third repeat): the person is asked, with what each printed and the points it was settling.
	if s := voucherRun(st, "2006", 4); s != "" || st.CycleAsk != "" {
		t.Fatalf("asked half way round: %q %q", s, st.CycleAsk)
	}
	voucherRun(st, "2009", 4)
	if !strings.Contains(st.CycleAsk, "back and forth") || !strings.Contains(st.CycleAsk, "voucher 1191's party") ||
		!strings.Contains(st.CycleAsk, "printed: Opened voucher 2006") || !strings.Contains(st.CycleAsk, "Nothing after this was checked") {
		t.Fatalf("not asked: %q", st.CycleAsk)
	}
}

func TestWorkThatMovesOnIsNoCycle(t *testing.T) {
	st := &operatorState{}
	// Waiting on something that changes: the same two commands, a different output each time.
	for i := 0; i < 3*cycleStopLaps; i++ {
		if s := st.noteRun("gui observe", fmt.Sprintf("Downloading %d%%", i*8)); s != "" {
			t.Fatalf("a changing screen taken for a round: %q", s)
		}
		if s := st.noteRun("sleep 5", ""); s != "" {
			t.Fatalf("a changing screen taken for a round: %q", s)
		}
	}
	// One command over and over is the repeat guard's, not this one's.
	st = &operatorState{}
	for i := 0; i < 3*cycleStopLaps; i++ {
		if s := st.noteRun("gui observe", "the same"); s != "" {
			t.Fatalf("one command taken for a round: %q", s)
		}
	}
	// Three and four in turn are a round too.
	for _, names := range [][]string{{"a", "b", "c"}, {"a", "b", "c", "d"}} {
		st = &operatorState{}
		var last string
		for i := 0; i < cycleWarnLaps; i++ {
			for _, n := range names {
				last = st.noteRun("gui open "+n, strings.ToUpper(n))
			}
		}
		want := fmt.Sprintf("run these %d commands in turn", len(names))
		if !strings.Contains(last, want) || !strings.Contains(last, "$ gui open a\nA") {
			t.Fatalf("%d in turn: %q", len(names), last)
		}
	}
	// A, B, A, B, A, B with each command differing only in its spacing is still the same round.
	st = &operatorState{}
	var last string
	for i := 0; i < cycleWarnLaps; i++ {
		st.noteRun("gui  open a", "A")
		last = st.noteRun("gui open   b", "B")
	}
	if !strings.Contains(last, "run these 2 commands") {
		t.Fatalf("spacing made it a new command: %q", last)
	}
}

// A step that writes something - typing a row, an office update, a file written - starts the count again: rounds that
// print the same each time can still be entering a row per round.
func TestAStepThatWritesSomethingStartsTheCountAgain(t *testing.T) {
	for _, writer := range []string{
		"gui type e4 'row'",
		"gui select e9 'Paid'",
		"office update book.xlsx --cell B2=4",
		"office create out.docx",
		"echo done > notes.txt",
		"gui key tab && gui type e5 x",
	} {
		st := &operatorState{}
		for i := 0; i < 2*cycleStopLaps; i++ {
			if s := st.noteRun(writer, "Typed."); s != "" || st.CycleAsk != "" {
				t.Fatalf("%q: a writing round taken for a cycle: %q %q", writer, s, st.CycleAsk)
			}
			if s := st.noteRun("gui observe", "Form\ne1 field"); s != "" || st.CycleAsk != "" {
				t.Fatalf("%q: a writing round taken for a cycle: %q %q", writer, s, st.CycleAsk)
			}
		}
	}
	// Moving about is not writing: clicks, keys and output to /dev/null or a descriptor go round like any look.
	for _, mover := range []string{"gui click e1", "gui key alt+tab", "ls > /dev/null 2>&1"} {
		if changesState(mover) {
			t.Errorf("%q taken for a write", mover)
		}
	}
}

// Found live (2026-10-27): the same cycle question went to the person twice in four minutes.
func TestTheCycleQuestionIsAskedOnceAGoalThenTheAnswerIsGivenBack(t *testing.T) {
	st := &operatorState{Hosted: true}
	round := func() string {
		var out string
		for i := 0; i < cycleStopLaps+1; i++ {
			out += st.noteRun("gui click e1 && gui observe", "screen A")
			out += st.noteRun("gui click e2 && gui observe", "screen B")
		}
		return out
	}
	round()
	if st.CycleAsk == "" || !st.CycleAsked {
		t.Fatalf("not asked the first time: %q", st.CycleAsk)
	}
	st.CycleAsk = ""
	st.PersonSaid = append(st.PersonSaid, "asked: back and forth - answered: Stop flipping. Note the names and go to the voucher form.")
	nudge := round()
	if st.CycleAsk != "" {
		t.Fatalf("asked a second time: %q", st.CycleAsk)
	}
	if !strings.Contains(nudge, "Stop flipping") || !strings.Contains(nudge, "not asked again") {
		t.Fatalf("the answer was not given back: %q", nudge)
	}
}
