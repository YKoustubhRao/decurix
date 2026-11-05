package skills

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"thebot/internal/llm"
	"thebot/internal/trace"
)

// After a job the trace has one call line per model call and one command line per command, in order, with the turns
// the calls were tagged with.
func TestTheTraceHasEveryCallAndCommandInOrder(t *testing.T) {
	chat := &taggedChat{replies: []string{"COMMAND:\necho first-out", "COMMAND:\necho Swordfish42", "DONE:\nboth echoed"}}
	dir := t.TempDir()
	w := trace.New(dir)
	rc := operatorContext(t, llm.Metered(trace.Calls(chat)))
	rc.Ctx = trace.WithWriter(llm.WithTags(context.Background(), llm.Tags{Task: "t_tr", Job: "jt"}), w)
	if _, err := NewOperator().Run(rc, Subtask{Step: "echo first-out, then the password is Swordfish42", Type: "operator"}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "t_tr.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "Swordfish42") {
		t.Fatalf("a secret the request gave reached the trace:\n%s", raw)
	}
	var kinds, turns []string
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	for sc.Scan() {
		var l struct {
			Kind, Turn, Purpose string
			Command             *struct{ Text, Guard string }
		}
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatal(err)
		}
		if l.Kind == "call" && l.Purpose != llm.PurposeOperatorTurn {
			continue // the operator's own checks are traced too; this test follows its turns
		}
		kinds = append(kinds, l.Kind)
		turns = append(turns, l.Turn)
		if l.Kind == "command" && l.Command.Guard != trace.GuardAllowed {
			t.Errorf("command %q guard %q", l.Command.Text, l.Command.Guard)
		}
	}
	wantKinds := []string{"call", "command", "call", "command", "call"}
	wantTurns := []string{"jt:1", "jt:1", "jt:2", "jt:2", "jt:3"}
	if strings.Join(kinds, ",") != strings.Join(wantKinds, ",") || strings.Join(turns, ",") != strings.Join(wantTurns, ",") {
		t.Fatalf("trace kinds %v turns %v, want %v %v", kinds, turns, wantKinds, wantTurns)
	}
	// The turns are the ones the calls themselves were tagged with (what the proxy's headers carried).
	var tagged []string
	for i, tg := range chat.tags {
		if chat.purposes[i] == llm.PurposeOperatorTurn {
			tagged = append(tagged, tg.Turn)
		}
	}
	if strings.Join(tagged, ",") != "jt:1,jt:2,jt:3" {
		t.Fatalf("calls tagged %v", tagged)
	}
}

// A command paused for a person's yes leaves a line saying so, with no exit code.
func TestAnAskedCommandIsTracedAsAsked(t *testing.T) {
	chat := &taggedChat{replies: []string{"COMMAND:\ngit push origin main"}}
	dir := t.TempDir()
	rc := operatorContext(t, chat)
	rc.Ctx = trace.WithWriter(llm.WithTags(context.Background(), llm.Tags{Task: "t_ask", Job: "ja"}), trace.New(dir))
	res, err := NewOperator().Run(rc, Subtask{Step: "push the branch", Type: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Checkpoint == nil {
		t.Fatalf("the push did not pause for approval: %+v", res)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "t_ask.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"guard":"asked","exit":null`) || !strings.Contains(string(raw), `"turn":"ja:1"`) {
		t.Fatalf("no asked line:\n%s", raw)
	}
}
