package skills

// The operator keeps each goal's transcript for the review pass after the task (internal/learnreview): the messages
// its last turn was sent, so the review can be sent them unchanged with one instruction after them.

import (
	"reflect"
	"testing"

	"thebot/internal/learnreview"
	"thebot/internal/llm"
)

func TestTheOperatorKeepsItsTranscriptForTheReview(t *testing.T) {
	chat := &scriptedChat{replies: []string{
		"COMMAND:\n" + forShell("echo hello", "Write-Output hello"),
		"DONE:\nIt said hello.",
	}}
	rc := operatorContext(t, chat)
	kp := learnreview.NewKeeper()
	rc.Ctx = learnreview.With(llm.WithTags(rc.Ctx, llm.Tags{Task: "t_keep", Job: "job7"}), kp)

	if _, err := NewOperator().Run(rc, Subtask{Step: "say hello", Type: "operator"}); err != nil {
		t.Fatal(err)
	}
	kept, ok := kp.Get("t_keep")
	if !ok {
		t.Fatal("the goal's transcript was not kept")
	}
	last := chat.calls[len(chat.calls)-1]
	// What the last turn was sent opens the kept transcript unchanged; the model's DONE follows it.
	if len(kept.Messages) < len(last) || !reflect.DeepEqual(kept.Messages[:len(last)], last) {
		t.Fatalf("the kept transcript is not what the last turn was sent")
	}
	if kept.Job != "job7" || kept.Model != "m" || kept.Request != "say hello" || kept.PrefixLen < 2 {
		t.Fatalf("kept = job %q model %q request %q prefix %d", kept.Job, kept.Model, kept.Request, kept.PrefixLen)
	}
	if kept.Messages[0].Role != "system" {
		t.Fatalf("the kept transcript does not open with the frame: %+v", kept.Messages[0])
	}
}
