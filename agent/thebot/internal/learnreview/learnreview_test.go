package learnreview

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"thebot/internal/llm"
)

// transcript is a small operator transcript: frame, contract (with the request), a command's output, the DONE.
func transcript() []llm.Message {
	return []llm.Message{
		{Role: "system", Content: "frame\nLesson (worker): Totals are shown with two decimals."},
		{Role: "user", Content: "contract... request: file the forms"},
		{Role: "assistant", Content: "$ gui look"},
		{Role: "user", Content: "Save is greyed out until the Date field is filled", Images: [][]byte{[]byte("png")}},
		{Role: "assistant", Content: "DONE: filed"},
	}
}

func TestKeepFilesTheTranscriptUnderTheTaskAndCopiesIt(t *testing.T) {
	kp := NewKeeper()
	ctx := With(llm.WithTags(context.Background(), llm.Tags{Task: "t_1", Job: "j1"}), kp)
	msgs := transcript()
	Keep(ctx, "m", msgs, 2, "file the forms")
	msgs[0].Content = "changed later"

	k, ok := kp.Get("t_1")
	if !ok || k.Job != "j1" || k.Model != "m" || k.PrefixLen != 2 || k.Messages[0].Content == "changed later" {
		t.Fatalf("kept = %+v, %v", k, ok)
	}
	if _, ok := kp.Get("t_2"); ok {
		t.Fatal("a task never kept was found")
	}
	// No task, or no keeper: nothing kept, nothing breaks.
	Keep(With(context.Background(), kp), "m", msgs, 2, "")
	Keep(llm.WithTags(context.Background(), llm.Tags{Task: "t_3"}), "m", msgs, 2, "")
	if _, ok := kp.Get(""); ok {
		t.Fatal("a transcript with no task was kept")
	}
}

func TestTheKeeperHoldsOnlyTheLastFewTasks(t *testing.T) {
	kp := NewKeeper()
	for i := range maxKeptTasks + 2 {
		task := "t_" + string(rune('a'+i))
		Keep(With(llm.WithTags(context.Background(), llm.Tags{Task: task}), kp), "m", transcript(), 2, "")
	}
	if _, ok := kp.Get("t_a"); ok {
		t.Fatal("the oldest task is still kept past the bound")
	}
	if _, ok := kp.Get("t_" + string(rune('a'+maxKeptTasks+1))); !ok {
		t.Fatal("the newest task was not kept")
	}
	big := []llm.Message{{Role: "user", Content: strings.Repeat("x", maxKeptBytes+1)}}
	Keep(With(llm.WithTags(context.Background(), llm.Tags{Task: "t_big"}), kp), "m", big, 0, "")
	if _, ok := kp.Get("t_big"); ok {
		t.Fatal("a transcript over the whole bound was kept")
	}
}

func TestMessagesKeepEveryKeptMessageAndAppendOneInstruction(t *testing.T) {
	k := Kept{Messages: transcript(), PrefixLen: 2}
	got := Messages(k)
	if len(got) != len(k.Messages)+1 || !reflect.DeepEqual(got[:len(k.Messages)], k.Messages) {
		t.Fatalf("the transcript did not open the review unchanged: %+v", got)
	}
	if last := got[len(got)-1]; last.Role != "user" || last.Content != Instruction {
		t.Fatalf("last message = %+v", last)
	}

	// Ending on a user message: the instruction joins it, everything before it unchanged.
	k.Messages = k.Messages[:4]
	got = Messages(k)
	if len(got) != 4 || !reflect.DeepEqual(got[:3], k.Messages[:3]) || !strings.HasSuffix(got[3].Content, Instruction) ||
		!strings.HasPrefix(got[3].Content, k.Messages[3].Content) || len(got[3].Images) != 1 {
		t.Fatalf("got %+v", got)
	}
	if k.Messages[3].Content == got[3].Content {
		t.Fatal("the kept message itself was changed")
	}
}

func TestDecideKeepsOnlyTheShapeAskedFor(t *testing.T) {
	answer := "```json\n" + `{"proposals": [
		{"kind": "app_note", "text": "Save stays greyed out  until the Date field is filled.", "why": "the form checks it", "evidence": ["Save is greyed out until the Date field is filled"]},
		{"kind": "rule", "text": "x", "evidence": ["a"]},
		{"kind": "lesson", "text": "no evidence", "evidence": []},
		{"kind": "lesson", "text": "y", "evidence": ["` + strings.Repeat("q", MaxQuoteChars+1) + `"]},
		{"kind": "lesson", "text": "two", "evidence": ["a"]},
		{"kind": "lesson", "text": "three", "evidence": ["b"]},
		{"kind": "lesson", "text": "four", "evidence": ["c"]}
	]}` + "\n```"
	d := Decide(answer)
	if d.Error != "" || len(d.Proposals) != MaxProposals || d.Dropped != 4 {
		t.Fatalf("decision = %+v", d)
	}
	if d.Proposals[0].Text != "Save stays greyed out until the Date field is filled." {
		t.Fatalf("text not collapsed: %q", d.Proposals[0].Text)
	}
	if d := Decide("nothing to propose"); d.Error == "" || len(d.Proposals) != 0 {
		t.Fatalf("an answer with no JSON = %+v", d)
	}
	if d := Decide(`{"proposals": []}`); d.Error != "" || len(d.Proposals) != 0 {
		t.Fatalf("an empty answer = %+v", d)
	}
}

func TestGroundFindsQuotesOnlyInWhatTheRunRead(t *testing.T) {
	k := Kept{Messages: transcript(), PrefixLen: 2, Request: "file the forms"}
	d := Decision{Proposals: []Proposal{
		{Evidence: []string{"Save is greyed out\n until the Date field is filled"}},
		{Evidence: []string{"Totals are shown with two decimals"}}, // the frame: not this run's
		{Evidence: []string{"DONE: filed"}},                        // the model's own words
		{Evidence: []string{"file the forms"}},                     // the request
		{Evidence: []string{"save is greyed out"}},                 // case is kept
	}}
	Ground(&d, Sources(k))
	want := []bool{true, false, false, true, false}
	for i, p := range d.Proposals {
		if p.InRun[0] != want[i] {
			t.Errorf("proposal %d: in run = %v, want %v", i, p.InRun[0], want[i])
		}
	}
}
