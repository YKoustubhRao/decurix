package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// On the local brain no request exceeds the window: with sixteen long outputs, every call stays under budget, the
// prefix (the contract and the task) is byte-identical on every call, and the goal ends within budget or not checked
// (review T-09).
func TestALocalGoalNeverOutgrowsTheWindow(t *testing.T) {
	var replies []string
	for i := 1; i <= 16; i++ {
		replies = append(replies, fmt.Sprintf("COMMAND:\ncat part%d.txt", i))
	}
	replies = append(replies, "DONE:\nRead all sixteen parts.")
	chat := &scriptedChat{replies: replies}
	rc := operatorContext(t, chat)
	for i := 1; i <= 16; i++ {
		body := strings.Repeat(fmt.Sprintf("part %d line of text\n", i), 4000/20)
		if err := os.WriteFile(filepath.Join(rc.WorkDir, fmt.Sprintf("part%d.txt", i)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res, err := (&OperatorSkill{MaxTurns: 40}).Run(rc, Subtask{Step: "read the sixteen parts", Type: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	first := chat.calls[0]
	t.Logf("prefix ~%d tokens (%d chars), calls %d", localTokens(first), messagesChars(first), len(chat.calls))
	for i, call := range chat.calls {
		if n := localTokens(call); n > localBudgetTokens {
			t.Fatalf("call %d sent ~%d tokens, over the local budget of %d", i+1, n, localBudgetTokens)
		}
		if len(call) < len(first) || !reflect.DeepEqual(call[:len(first)], first) {
			t.Fatalf("call %d does not start with the first call's prefix", i+1)
		}
	}
	if !res.Verified && !strings.Contains(res.Output, "Not checked") {
		t.Fatalf("the goal ended neither within budget nor as not checked: %+v", res)
	}
	if len(chat.calls) < 10 {
		t.Errorf("only %d calls: the bound ended the goal before masking could help", len(chat.calls))
	}
}
