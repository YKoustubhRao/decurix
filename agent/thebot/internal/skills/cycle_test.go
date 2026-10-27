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
	for round := 1; round <= cycleRounds; round++ {
		said = append(said, voucherRun(st, "2006", round), voucherRun(st, "2009", round))
	}
	for _, s := range said[:len(said)-1] {
		if s != "" {
			t.Fatalf("a note before %d rounds: %q", cycleRounds, s)
		}
	}
	last := said[len(said)-1]
	if !strings.Contains(last, "run these 2 commands in turn 4 times") || !strings.Contains(last, "Opened voucher 2006") ||
		!strings.Contains(last, "Do not go round them again") {
		t.Fatalf("the round is not shown whole: %q", last)
	}
	if st.CycleAsk != "" || st.CycleNudges != 1 {
		t.Fatalf("asked already: %q (%d)", st.CycleAsk, st.CycleNudges)
	}
	// Round again: the person is asked, with the points it was settling.
	for round := 5; round <= 4+cycleRounds; round++ {
		voucherRun(st, "2006", round)
		voucherRun(st, "2009", round)
	}
	if !strings.Contains(st.CycleAsk, "back and forth") || !strings.Contains(st.CycleAsk, "voucher 1191's party") {
		t.Fatalf("not asked: %q", st.CycleAsk)
	}
}

func TestWorkThatMovesOnIsNoCycle(t *testing.T) {
	st := &operatorState{}
	// Waiting on something that changes: the same two commands, a different output each time.
	for i := 0; i < 3*cycleRounds; i++ {
		if s := st.noteRun("gui observe", fmt.Sprintf("Downloading %d%%", i*8)); s != "" {
			t.Fatalf("a changing screen taken for a round: %q", s)
		}
		if s := st.noteRun("sleep 5", ""); s != "" {
			t.Fatalf("a changing screen taken for a round: %q", s)
		}
	}
	// One command over and over is the repeat guard's, not this one's.
	st = &operatorState{}
	for i := 0; i < 3*cycleRounds; i++ {
		if s := st.noteRun("gui observe", "the same"); s != "" {
			t.Fatalf("one command taken for a round: %q", s)
		}
	}
	// Three in turn are a round too.
	st = &operatorState{}
	var last string
	for i := 0; i < cycleRounds; i++ {
		st.noteRun("gui open a", "A")
		st.noteRun("gui open b", "B")
		last = st.noteRun("gui open c", "C")
	}
	if !strings.Contains(last, "run these 3 commands in turn") || !strings.Contains(last, "$ gui open a\nA") {
		t.Fatalf("three in turn: %q", last)
	}
}

// Found live (2026-10-27): the same cycle question went to the person twice in four minutes.
func TestTheCycleQuestionIsAskedOnceAGoalThenTheAnswerIsGivenBack(t *testing.T) {
	st := &operatorState{Hosted: true}
	round := func() string {
		var out string
		for i := 0; i < cycleRounds+1; i++ {
			if o := st.noteRun("gui click e1 && gui observe", "screen A"); o != "" {
				out = o
			}
			if o := st.noteRun("gui click e2 && gui observe", "screen B"); o != "" {
				out = o
			}
		}
		return out
	}
	round()
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
