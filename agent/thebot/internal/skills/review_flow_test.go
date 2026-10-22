package skills

import (
	"context"
	"strings"
	"testing"

	"thebot/internal/llm"
)

// The review's flow around approvals and the person's words (independent review of the harness, 2026-10-22).

const gatedGui = "echo 'NEEDS_APPROVAL: Approval needed before I click button \"FILE\" (e35): it files something on your behalf. Nothing was clicked or sent.'\nexit 3\n"

func TestAListWithACommandAtAnApprovalRunsTheCommandFirst(t *testing.T) {
	stubWorkerTools(t, gatedGui, "", "")
	_, chat := hostedRun(t, []string{
		"COMMAND:\necho register row 455 CANCELLED", "COMMAND:\necho two", "COMMAND:\necho three", "COMMAND:\necho four",
		"COMMAND:\ngui click e35",
		"JUDGEMENTS:\n- Bill 455 counted as cancelled | backed by: \"register row 455 CANCELLED\" (register)\n\nCOMMAND:\necho fixed-first",
		"DONE:\nStopped here.",
	})
	ran := false
	for _, call := range chat.calls {
		if strings.Contains(call[len(call)-1].Content, "fixed-first") {
			ran = true
		}
	}
	if !ran {
		t.Fatal("the command sent beside the list was dropped and the action put to the person instead")
	}
}

func TestAGateStoppingAPointBeingSettledCarriesThePointsToTheApproval(t *testing.T) {
	stubWorkerTools(t, gatedGui, "", "")
	res, _ := hostedRun(t, []string{
		"COMMAND:\necho register row 455 CANCELLED", "COMMAND:\necho two", "COMMAND:\necho three", "COMMAND:\necho four",
		"COMMAND:\ngui click e35",
		"JUDGEMENTS:\n- Rate taken as 18% | not backed: earlier months",
		`{"issues": [{"what": "Rate 18% is in no document read", "settle": "recheck"}]}`,
		"COMMAND:\ngui click e36", // settling, and stopped by the gate again
	})
	reason, _ := res.Meta["approvalReason"].(string)
	if res.Meta["needsApproval"] != true || !strings.Contains(reason, "Still open after an independent review") ||
		!strings.Contains(reason, "Rate 18% is in no document read") {
		t.Fatalf("result = %+v", res.Meta)
	}
}

func TestAQuoteIsFoundOnlyAsWholeWords(t *testing.T) {
	seen := " " + plainWords("the rate 50 applies to bills of September and October") + " "
	if quoteSeen("the rate 5 applies to bills", seen) {
		t.Error("\"rate 5\" was found in \"rate 50\"")
	}
	if !quoteSeen("the rate 50 applies to bills", seen) {
		t.Error("the whole quote was not found")
	}
}

func TestThePersonsWordsOutliveEveryCompaction(t *testing.T) {
	st := longGoal(30)
	st.PersonSaid = []string{"Asked: which rate?\n  They said: the rent is paid in cash"}
	o, rc := &OperatorSkill{}, RunContext{Ctx: context.Background(), Provider: &notesProvider{}, OnLog: func(string) {}}
	o.compact(rc, st, "first")
	for i := 0; i < 30; i++ {
		st.Messages = append(st.Messages, llm.Message{Role: "assistant", Content: "COMMAND:\necho x"},
			llm.Message{Role: "user", Content: "exit_code: 0"})
	}
	o.compact(rc, st, "second")
	if note := st.Messages[st.prefixEnd()].Content; !strings.Contains(note, "the rent is paid in cash") {
		t.Fatalf("the second compaction lost what the person said: %q", note)
	}
	if facts := st.reviewFacts("done", nil, ""); !strings.Contains(facts, "the rent is paid in cash") {
		t.Error("the reviewer was not shown what the person said")
	}
}

func TestTheReviewerSeesTheLessonsOfALongFrame(t *testing.T) {
	frame := "intro\n" + strings.Repeat("persona and tools text. ", 2000) + "\nLESSON: rent under reverse charge is paid in cash."
	st := &operatorState{Request: "file it", PrefixLen: 1, Messages: []llm.Message{{Role: "system", Content: frame}}}
	if facts := st.reviewFacts("done", nil, ""); !strings.Contains(facts, "rent under reverse charge is paid in cash") {
		t.Error("the lessons at the end of a long frame were cut off")
	}
}
