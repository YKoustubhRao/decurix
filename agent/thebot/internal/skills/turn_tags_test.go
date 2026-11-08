package skills

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"

	"thebot/internal/llm"
)

// taggedChat answers from a script and keeps the tags and purpose of every call.
type taggedChat struct {
	mu       sync.Mutex
	replies  []string
	tags     []llm.Tags
	purposes []string
}

func (c *taggedChat) Name() string                     { return "tagged" }
func (c *taggedChat) IsReachable(context.Context) bool { return true }
func (c *taggedChat) Chat(ctx context.Context, _ string, _ []llm.Message, opts llm.Options) (llm.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tags = append(c.tags, llm.TagsFrom(ctx))
	c.purposes = append(c.purposes, opts.Purpose)
	if len(c.tags) > len(c.replies) {
		return llm.Result{Text: "DONE: out of script"}, nil
	}
	return llm.Result{Text: c.replies[len(c.tags)-1]}, nil
}

// Each operator turn's model call carries "<job>:<n>", n going up by one a turn, with the job's task.
func TestOperatorTurnsAreTaggedAndCountUp(t *testing.T) {
	chat := &taggedChat{replies: []string{"COMMAND:\necho one", "COMMAND:\necho two", "DONE:\ntwo things echoed"}}
	rc := operatorContext(t, chat)
	rc.Ctx = llm.WithTags(context.Background(), llm.Tags{Task: "t_5", Job: "jx"})
	if _, err := NewOperator().Run(rc, Subtask{Step: "echo two things", Type: "operator"}); err != nil {
		t.Fatal(err)
	}
	var turns []string
	for i, tag := range chat.tags {
		if chat.purposes[i] != llm.PurposeOperatorTurn {
			continue
		}
		if tag.Task != "t_5" || tag.Job != "jx" {
			t.Fatalf("call %d lost the job's tags: %+v", i, tag)
		}
		turns = append(turns, tag.Turn)
	}
	want := []string{"jx:1", "jx:2", "jx:3"}
	if len(turns) != len(want) {
		t.Fatalf("operator turns tagged %v, want %v", turns, want)
	}
	for i := range want {
		if turns[i] != want[i] {
			t.Fatalf("operator turns tagged %v, want %v", turns, want)
		}
	}
}

// Two operator runs in one job never number a turn twice: the counter is the job's.
func TestTwoOperatorRunsInOneJobShareTheTurnCount(t *testing.T) {
	chat := &taggedChat{replies: []string{"DONE:\nfirst", "DONE:\nsecond"}}
	rc := operatorContext(t, chat)
	rc.Ctx = llm.WithTags(context.Background(), llm.Tags{Job: "jy"})
	for _, step := range []string{"say first", "say second"} {
		if _, err := NewOperator().Run(rc, Subtask{Step: step, Type: "operator"}); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for i, tag := range chat.tags {
		if chat.purposes[i] != llm.PurposeOperatorTurn {
			continue
		}
		if seen[tag.Turn] {
			t.Fatalf("turn %q was used twice: %+v", tag.Turn, chat.tags)
		}
		seen[tag.Turn] = true
	}
	if !seen["jy:1"] || !seen["jy:2"] {
		t.Fatalf("turns = %v", seen)
	}
}

// A command the operator runs is told its task and turn (PLAN-P2 C3); a job with no task tells it neither.
func TestACommandSeesItsTaskAndTurn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("reads the environment with env")
	}
	run := func(tags llm.Tags) string {
		// Only the DECURIX_ variables: a CI runner's whole environment is long enough that the output kept for the
		// model is cut before them (CI on main, 2026-11-05).
		chat := &scriptedChat{replies: []string{"COMMAND:\nenv | grep '^DECURIX_' || true", "DONE:\nlisted"}}
		rc := operatorContext(t, chat)
		rc.Ctx = llm.WithTags(context.Background(), tags)
		if _, err := NewOperator().Run(rc, Subtask{Step: "list the environment", Type: "operator"}); err != nil {
			t.Fatal(err)
		}
		if len(chat.calls) < 2 {
			t.Fatalf("the command's output never reached the model: %d calls", len(chat.calls))
		}
		return chat.calls[1][len(chat.calls[1])-1].Content
	}
	out := run(llm.Tags{Task: "t_env", Job: "jz"})
	if !strings.Contains(out, "DECURIX_TASK_ID=t_env") || !strings.Contains(out, "DECURIX_TURN_ID=jz:1") {
		t.Fatalf("the command did not see its task and turn:\n%s", out)
	}
	out = run(llm.Tags{})
	if strings.Contains(out, "DECURIX_TASK_ID=") || strings.Contains(out, "DECURIX_TURN_ID=") {
		t.Fatalf("a job with no task told the command one:\n%s", out)
	}
}

func TestGuiEnvCarriesTheTaskAndTurnOnlyWhenSet(t *testing.T) {
	has := func(env []string, prefix string) bool {
		for _, kv := range env {
			if strings.HasPrefix(kv, prefix) {
				return true
			}
		}
		return false
	}
	st := &operatorState{taskID: "t_1", turnID: "j:3"}
	if env := st.guiEnv(); !has(env, "DECURIX_TASK_ID=t_1") || !has(env, "DECURIX_TURN_ID=j:3") {
		t.Fatalf("guiEnv = %v", env)
	}
	if env := (&operatorState{}).guiEnv(); has(env, "DECURIX_TASK_ID=") || has(env, "DECURIX_TURN_ID=") {
		t.Fatalf("guiEnv with no task = %v", env)
	}
}
