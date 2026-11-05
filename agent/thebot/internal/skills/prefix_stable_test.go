package skills

import (
	"reflect"
	"testing"
)

// The prompt prefix is byte-identical on every call of a run (CLAUDE.md: Ollama reloads otherwise): each later call's
// messages begin with exactly the first call's fixed part - system frame, history and contract with the task. Checked
// over a run that fails, is pushed back and asks (the paths this round's changes touch).
func TestThePromptPrefixIsTheSameOnEveryCall(t *testing.T) {
	chat := &scriptedChat{replies: []string{
		"COMMAND:\nno_such_program_here",
		"DONE:\nanswer from memory",
		"COMMAND:\ncat /no/such/file",
		"DONE:\nstill from memory",
	}}
	if _, err := NewOperator().Run(operatorContext(t, chat), Subtask{Step: "what is the version?", Type: "operator"}); err != nil {
		t.Fatal(err)
	}
	if len(chat.calls) < 3 {
		t.Fatalf("only %d calls", len(chat.calls))
	}
	first := chat.calls[0]
	prefix := len(first) // the first call is the prefix alone
	for i, call := range chat.calls[1:] {
		if len(call) < prefix || !reflect.DeepEqual(call[:prefix], first) {
			t.Fatalf("call %d does not start with the first call's messages", i+2)
		}
	}
}
