package skills

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"thebot/internal/llm"
	"thebot/internal/trace"
	"thebot/internal/withheld"
)

// logsOf runs the operator on step with chat's replies and returns its job log and the trace.
func logsOf(t *testing.T, chat llm.Provider, ctx context.Context, step string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	rc := operatorContext(t, llm.Metered(trace.Calls(chat)))
	var mu sync.Mutex
	var logged []string
	rc.OnLog = func(l string) { mu.Lock(); logged = append(logged, l); mu.Unlock() }
	rc.Ctx = trace.WithWriter(llm.WithTags(ctx, llm.Tags{Task: "t_em", Job: "je"}), trace.New(dir))
	if _, err := NewOperator().Run(rc, Subtask{Step: step, Type: "operator"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "t_em.jsonl"))
	mu.Lock()
	defer mu.Unlock()
	return strings.Join(logged, "\n"), string(raw)
}

// B3: the step line ("$ ...", which becomes "Running: ..." in the chat) withholds what soul knows by structure, as
// the trace does: here a value the request gives after a password's name, with no secrets sent on the job.
func TestTheStepLineWithholdsWhatTheTraceWithholds(t *testing.T) {
	chat := &taggedChat{replies: []string{"COMMAND:\necho first-out", "COMMAND:\necho Swordfish42", "DONE:\nboth echoed"}}
	logged, traced := logsOf(t, chat, context.Background(), "echo first-out, then the password is Swordfish42")
	if strings.Contains(traced, "Swordfish42") || strings.Contains(logged, "Swordfish42") {
		t.Fatalf("trace holds it: %v; step log holds it: %v\n%s", strings.Contains(traced, "Swordfish42"),
			strings.Contains(logged, "Swordfish42"), logged)
	}
	if !strings.Contains(logged, "$ echo "+secretShown) {
		t.Fatalf("the step line is not there, withheld:\n%s", logged)
	}
}

// B3: a value typed into a field the screen lists as a password field is withheld from the step line, with no
// secrets sent on the job.
func TestATypedPasswordIsWithheldFromTheStepLine(t *testing.T) {
	st := &operatorState{Request: "sign in as mira.k"}
	st.noteSeen("Screen 1280x720. Active window: \"Sign in - Chromium\"\ne4 textbox \"Username\"\ne5 password text \"Password\"")
	command := "gui type e5 'quince-orchard-77'"
	if got := st.withholdFor(command)(command); strings.Contains(got, "quince") || !strings.Contains(got, "gui type e5") {
		t.Fatalf("withholdFor = %q", got)
	}
}

// B3: the job's own values are withheld by the same function, before the structural ones.
func TestTheJobsValuesGoThroughTheSameFilter(t *testing.T) {
	chat := &taggedChat{replies: []string{"COMMAND:\necho larch-meadow-19", "DONE:\nechoed"}}
	ctx := withheld.With(context.Background(), []string{"larch-meadow-19"})
	logged, traced := logsOf(t, chat, ctx, "echo the reply")
	if strings.Contains(logged, "larch") || strings.Contains(traced, "larch") {
		t.Fatalf("a job's value left in the step log or trace:\n%s\n%s", logged, traced)
	}
}

// B4: what a command printed is withheld before it is noted in the run's outputs file, and the file is the worker's
// alone.
func TestCommandOutputsAreWithheldAndPrivate(t *testing.T) {
	root := t.TempDir()
	taskFile := filepath.Join(root, "task")
	if err := os.WriteFile(taskFile, []byte("t8out"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DECURIX_TASK_FILE", taskFile)
	t.Setenv("DECURIX_DIR", filepath.Join(root, "state"))
	st := &operatorState{jobSecrets: withheld.Clean([]string{"larch-meadow-19"}), Request: "the password is Swordfish42"}
	command := "cat plans.txt"
	noteCommandOutput(command, "zip -P larch-meadow-19 out.zip; then Swordfish42; total 41.00", st.withholdFor(command))
	path := filepath.Join(root, "state", "computer", "evidence", "t8out.outputs")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "larch") || strings.Contains(string(data), "Swordfish42") || !strings.Contains(string(data), "41.00") {
		t.Fatalf("outputs = %s", data)
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
			t.Fatalf("outputs mode %v, want 0600", info.Mode().Perm())
		}
	}
}

// B3: an approval's question quotes the action withheld as the step line is; the action it matches stays whole.
func TestAnApprovalQuotesTheWithheldAction(t *testing.T) {
	st := &operatorState{Request: "the password is Swordfish42", Pending: "git push https://u:Swordfish42@example.test main"}
	res := NewOperator().pause(Subtask{Step: "push"}, st, "it publishes")
	shown, _ := res.Meta["actionShown"].(string)
	if strings.Contains(shown, "Swordfish42") || strings.Contains(res.Output, "Swordfish42") {
		t.Fatalf("shown %q output %q", shown, res.Output)
	}
	if action, _ := res.Meta["action"].(string); action != st.Pending {
		t.Fatalf("the action to match was changed: %q", action)
	}
}
