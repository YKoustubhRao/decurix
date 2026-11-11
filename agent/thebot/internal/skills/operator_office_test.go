package skills

import (
	"strings"
	"testing"

	"thebot/internal/llm"
)

// Found live (2026-10-14): asked for a review deck, the operator had no tool
// that made a .pptx and nothing told it how to change one it was given. The
// contract names both, the way it names the workbook and document commands.
func TestOperatorContractTeachesMakingAndChangingADeck(t *testing.T) {
	for _, want := range []string{
		"office create <file.pptx> - with {\"slides\":",
		"office update <file.pptx> - and {\"edits\":",
		"\"chart\": {\"type\": \"column\"",
		"never rebuild a deck someone gave you",
		"PowerPoint, email",
	} {
		if !strings.Contains(operatorContract, want) {
			t.Errorf("the operator's contract does not say %q", want)
		}
	}
}

// Found live (freeze 2.4, 2026-11-11): to re-check a JSON file it had exported, the worker piped it into inline
// python3; the command guard holds code piped into an interpreter for the person, and the case ended there. The
// contract sends a re-read to the read tools, in the fixed part every call starts with, and the missing-jq hint no
// longer suggests the piped form.
func TestOperatorContractReadsAFileWithItsReadTools(t *testing.T) {
	for _, want := range []string{
		"use your read tools: office read <file>, jq '.key' <file>, or cat, head and grep on the file",
		"never pipe a file into an interpreter",
	} {
		if !strings.Contains(operatorContract, want) {
			t.Errorf("the operator's contract does not say %q", want)
		}
	}
	chat := &scriptedChat{replies: []string{"COMMAND:\nno_such_program_here", "DONE:\nnothing"}}
	if _, err := NewOperator().Run(operatorContext(t, chat), Subtask{Step: "check the export", Type: "operator"}); err != nil {
		t.Fatal(err)
	}
	if len(chat.calls) < 2 {
		t.Fatalf("only %d calls", len(chat.calls))
	}
	in := func(call []llm.Message) bool {
		for _, m := range call {
			if strings.Contains(m.Content, "never pipe a file into an interpreter") {
				return true
			}
		}
		return false
	}
	for i, call := range chat.calls {
		if !in(call[:len(chat.calls[0])]) {
			t.Errorf("call %d's fixed prefix does not carry the read-tools rule", i+1)
		}
	}
	hint := missingProgramHint("bash: line 1: jq: command not found", "jq . out.json")
	if strings.Contains(hint, "json,sys") || !strings.Contains(hint, "never pipe the file into python3") {
		t.Errorf("jq hint = %q", hint)
	}
}

// Found live (round 5 P6, 2026-10-20, twice): an email with no order number was left "Need order lookup" though the
// system's order list had the sender's company and the product. The task rules say to find it by what is given.
func TestOperatorContractFindsARecordByWhatTheMessageGives(t *testing.T) {
	for _, want := range []string{
		"when its source gives no reference is found by what the source does give",
		"search or filter the system's list",
		"only when several match or none does",
	} {
		if !strings.Contains(taskRules, want) {
			t.Errorf("the operator's task rules do not say %q", want)
		}
	}
}

// Found live (round 5 P1, 2026-10-20): a bill's code wrapped under its row was paired with the next row's rate, and
// two bills were flagged with GST rate mismatches they did not have.
func TestOperatorTaskRulesReadAWrappedRowAsItsOwn(t *testing.T) {
	for _, want := range []string{
		"a code on such a line belongs to the row above",
		"that row's figures are its own",
	} {
		if !strings.Contains(taskRules, want) {
			t.Errorf("the operator's task rules do not say %q", want)
		}
	}
}
