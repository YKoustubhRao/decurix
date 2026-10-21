package skills

import (
	"strings"
	"testing"

	"thebot/internal/llm"
)

// Every turn re-sends the conversation so far unchanged and adds to its end: the prefix a provider caches (Azure on
// its own, Claude where the proxy marks it) stays byte-identical, so a turn pays full price only for what is new.
func TestEachTurnReSendsTheLastOneUnchanged(t *testing.T) {
	var logged []string
	chat := &scriptedChat{replies: []string{
		"COMMAND:\n" + forShell("echo one", "echo one"),
		"COMMAND:\n" + forShell("echo two", "echo two"),
		"COMMAND:\n" + forShell("echo three", "echo three"),
		"DONE:\nSaid one, two and three.",
	}}
	rc := operatorContext(t, chat)
	rc.OnLog = func(line string) { logged = append(logged, line) }
	if _, err := NewOperator().Run(rc, Subtask{Step: "say one, two and three", Type: "operator"}); err != nil {
		t.Fatal(err)
	}
	if len(chat.calls) < 4 {
		t.Fatalf("calls = %d", len(chat.calls))
	}
	for i := 1; i < len(chat.calls); i++ {
		before, now := chat.calls[i-1], chat.calls[i]
		if len(now) <= len(before) {
			t.Fatalf("turn %d sent %d messages after %d", i+1, len(now), len(before))
		}
		for j := range before {
			if before[j].Role != now[j].Role || before[j].Content != now[j].Content || len(before[j].Images) != len(now[j].Images) {
				t.Errorf("turn %d changed message %d of the prefix: %q -> %q", i+1, j, truncateText(before[j].Content, 80),
					truncateText(now[j].Content, 80))
			}
		}
	}
	usage := 0
	for _, line := range logged {
		if strings.Contains(line, "usage: turn ") && strings.Contains(line, "est. system") {
			usage++
		}
	}
	if usage != len(chat.calls) {
		t.Errorf("usage lines = %d for %d turns", usage, len(chat.calls))
	}
}

func TestATurnsPromptIsMeasuredByPart(t *testing.T) {
	st := &operatorState{Request: strings.Repeat("r", 400), Messages: []llm.Message{
		{Role: "system", Content: strings.Repeat("s", 100)},
		{Role: "user", Content: strings.Repeat("c", 600) + strings.Repeat("r", 400)},
		{Role: "assistant", Content: strings.Repeat("a", 40)},
		{Role: "user", Content: strings.Repeat("o", 2000), Images: [][]byte{{1}}},
		{Role: "assistant", Content: strings.Repeat("a", 40)},
		{Role: "user", Content: strings.Repeat("n", 300)},
	}}
	got := measurePrompt(st)
	want := promptParts{System: 100, Contract: 600, Request: 400, Replies: 80, Earlier: 2000, Latest: 300, Pictures: 1}
	if got != want {
		t.Errorf("parts = %+v, want %+v", got, want)
	}
	line := turnUsage(3, st, llm.Result{PromptTokens: 900, CachedTokens: 800, ResponseTokens: 12})
	for _, part := range []string{"turn 3 prompt 900 tokens (cached 800)", "contract 150", "request 100", "earlier outputs/notes 500"} {
		if !strings.Contains(line, part) {
			t.Errorf("line lacks %q: %s", part, line)
		}
	}
}

// What the guard traces against is everything a command showed (st.Seen), kept apart from what is re-sent to the
// model: a value from an early output stays traceable whatever happens to the conversation (were old outputs ever
// shortened, the guard must not start refusing correct values).
func TestTheGuardTracesFromWhatWasSeenNotFromWhatIsReSent(t *testing.T) {
	st := groundingState("Enter the PO from the customer.", "office read /w/PO-7781.pdf")
	st.noteSeen("PO HH-PO-7781 from Harbour Hotels Group\nTotal: 61,950.00\nDeliver by 24/10/2026")
	for i := 0; i < 30; i++ {
		st.noteSeen(strings.Repeat("later output line\n", 200))
	}
	st.Messages = nil // nothing of the early output left in the conversation
	for _, cmd := range []string{`gui type e5 "HH-PO-7781"`, `gui type e6 "61950"`, `gui type e7 "Harbour Hotels Group"`} {
		if n := groundingNudge(st, cmd); n != "" {
			t.Errorf("%s was refused although a command showed it: %q", cmd, n)
		}
	}
}
