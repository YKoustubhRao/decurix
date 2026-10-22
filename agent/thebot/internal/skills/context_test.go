package skills

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"thebot/internal/llm"
)

// notesProvider answers every call with the same notes.
type notesProvider struct{ calls int }

func (p *notesProvider) Name() string                         { return "notes" }
func (p *notesProvider) IsReachable(ctx context.Context) bool { return true }
func (p *notesProvider) Chat(_ context.Context, _ string, _ []llm.Message, _ llm.Options) (llm.Result, error) {
	p.calls++
	return llm.Result{Text: "NOTES: GSTR-1 saved; B2CS 254237.29 from COMPUTATION!B72; next the HSN table."}, nil
}

func longGoal(n int) *operatorState {
	st := &operatorState{Hosted: true, Request: "file September", PrefixLen: 2}
	st.Messages = []llm.Message{{Role: "system", Content: "frame"}, {Role: "user", Content: "contract: file September"}}
	for i := 0; i < n; i++ {
		st.Messages = append(st.Messages, llm.Message{Role: "assistant", Content: fmt.Sprintf("COMMAND:\ngui click e%d", i)})
		content := fmt.Sprintf("Command execution result:\n\nexit_code: 0\n\noutput:\nclicked e%d", i)
		if i == 3 {
			content = "The human replied: the rent is paid in cash\n\nContinue the task."
		}
		st.Messages = append(st.Messages, llm.Message{Role: "user", Content: content})
		st.noteLedger(fmt.Sprintf("gui click e%d", i), shellResult{Output: fmt.Sprintf("Clicked e%d", i)})
	}
	return st
}

func TestALongGoalIsShortenedToItsPrefixNotesRecordAndNewestSteps(t *testing.T) {
	st := longGoal(30)
	st.ScreenMsgs = []int{5, len(st.Messages) - 1}
	provider := &notesProvider{}
	(&OperatorSkill{}).compact(RunContext{Ctx: context.Background(), Provider: provider, OnLog: func(string) {}}, st, "test")

	if provider.calls != 1 {
		t.Fatalf("notes asked %d times", provider.calls)
	}
	if st.Messages[0].Content != "frame" || st.Messages[1].Content != "contract: file September" {
		t.Error("the prefix changed")
	}
	note := st.Messages[2].Content
	for _, want := range []string{"PROGRESS SO FAR", "B2CS 254237.29 from COMPUTATION!B72", "$ gui click e0 -> exit 0 · Clicked e0",
		"The human replied: the rent is paid in cash"} {
		if !strings.Contains(note, want) {
			t.Errorf("progress note lacks %q", want)
		}
	}
	if st.Messages[3].Role != "assistant" || len(st.Messages) != 3+keepRecentMessages {
		t.Errorf("kept %d messages, first kept %q", len(st.Messages)-3, st.Messages[3].Role)
	}
	if len(st.ScreenMsgs) != 1 || st.ScreenMsgs[0] != len(st.Messages)-1 {
		t.Errorf("screen indexes not moved with their messages: %v", st.ScreenMsgs)
	}
	if st.Compactions != 1 {
		t.Error("not counted")
	}
}

func TestAShortGoalIsLeftAsItIs(t *testing.T) {
	st := longGoal(4)
	before := len(st.Messages)
	(&OperatorSkill{}).compact(RunContext{Ctx: context.Background(), Provider: &notesProvider{}, OnLog: func(string) {}}, st, "test")
	if len(st.Messages) != before {
		t.Error("a short goal was shortened")
	}
}

func TestOnlyAWindowThatMadeProgressGetsAnotherBudget(t *testing.T) {
	st := &operatorState{Hosted: true, CommandsSucceeded: 40}
	if !st.mayContinue(30) {
		t.Error("ten commands worked in the window: should go on")
	}
	if st.mayContinue(38) {
		t.Error("two commands worked: should not go on")
	}
	st.Continuations = maxContinuations
	if st.mayContinue(0) {
		t.Error("no budgets left")
	}
	if (&operatorState{CommandsSucceeded: 40}).mayContinue(0) {
		t.Error("a local model is left as it was")
	}
}
