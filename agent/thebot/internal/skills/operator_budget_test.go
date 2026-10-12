package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An operator close to the end of its turns is told so once, so that what
// blocks it reaches the person as a question rather than as a failure.
func TestOperatorIsToldWhenItsTurnsAreNearlySpent(t *testing.T) {
	turns := 4 * closingTurns
	chat := &scriptedChat{}
	for i := 0; i < turns; i++ {
		chat.replies = append(chat.replies, fmt.Sprintf("COMMAND:\necho step-%d", i))
	}

	op := &OperatorSkill{MaxTurns: turns}
	res, _ := op.Run(operatorContext(t, chat), Subtask{Step: "print the steps", Type: "operator"})

	warned := turns - closingTurns
	note := fmt.Sprintf("You have %d turns left", closingTurns)
	for i, call := range chat.calls {
		told := strings.Contains(call[len(call)-1].Content, note)
		if told != (i == warned) {
			t.Errorf("turn %d: told about the budget = %v, want it said once, at turn %d", i, told, warned)
		}
	}
	// Nothing touched the screen: the plain failure stands, for the scheduler to reason about.
	if res.Meta["question"] != nil || !strings.Contains(res.Output, "ran out of turns") {
		t.Errorf("result = %q, meta %v", res.Output, res.Meta)
	}
}

// Out of turns after working on the screen: the person is asked, with what
// the model says is done so far, and the goal is kept for their reply. A
// plain failure there was re-planned as the whole entry made again.
func TestOperatorOutOfTurnsOnTheScreenAsksRatherThanFails(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gui"), []byte("#!/bin/sh\necho \"Clicked $2.\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	chat := &scriptedChat{replies: []string{
		"COMMAND:\ngui click e1",
		"COMMAND:\necho two",
		"COMMAND:\necho three",
		"HELP:\nThe three rows are entered and not saved: the form asks for a storage place, which the document does not give.",
	}}
	op := &OperatorSkill{MaxTurns: 3}
	res, _ := op.Run(operatorContext(t, chat), Subtask{Step: "enter the document in the open form", Type: "operator"})

	question, _ := res.Meta["question"].(string)
	if !strings.Contains(question, "ran out of steps") || !strings.Contains(question, "storage place") {
		t.Fatalf("question = %q, want the state of the work put to the person", question)
	}
	if res.Checkpoint == nil || res.Verified {
		t.Errorf("checkpoint kept = %v, verified = %v; the goal must be resumable and not a success", res.Checkpoint != nil, res.Verified)
	}
	last := chat.calls[3][len(chat.calls[3])-1].Content
	if !strings.Contains(last, "No turns are left") {
		t.Errorf("the closing call did not say the turns were spent:\n%s", last)
	}
}
