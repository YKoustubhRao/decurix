package skills

import (
	"strings"
	"testing"
)

// A later job of a task (one the final check sent back) starts its own goal: the person's replies from before it are
// in its review, each with its question, and the reply's words (never the question's) are a source for typed values.
func TestAGoalStartsWithThePersonsEarlierReplies(t *testing.T) {
	st := &operatorState{Request: "Enter the bill"}
	st.seedEarlierSaid([]Said{
		{Asked: "Which cost centre should I select for this bill?", Said: "Use the cost centre of the supplier's state: CC-27 for Maharashtra."},
		{Asked: "", Said: "   "},
	})
	if len(st.PersonSaid) != 1 {
		t.Fatalf("PersonSaid = %q", st.PersonSaid)
	}
	facts := st.reviewFacts("DONE: entered", nil, "")
	if !strings.Contains(facts, "WHAT THE PERSON SAID IN THIS GOAL") || !strings.Contains(facts, "Asked: Which cost centre") ||
		!strings.Contains(facts, "They said: Use the cost centre of the supplier's state: CC-27") {
		t.Fatalf("the review is not shown the reply with its question:\n%s", facts)
	}
	if !strings.Contains(st.Seen, "CC-27 for Maharashtra") {
		t.Fatalf("the reply is not a source: %q", st.Seen)
	}
	if strings.Contains(st.Seen, "Which cost centre should I select") {
		t.Fatalf("the worker's own question became a source: %q", st.Seen)
	}
}

// What a job is sent is bounded: at most MaxEarlierSaid replies, each cut to MaxEarlierSaidText.
func TestEarlierRepliesAreBounded(t *testing.T) {
	var said []Said
	for i := 0; i < MaxEarlierSaid+5; i++ {
		said = append(said, Said{Said: strings.Repeat("x", MaxEarlierSaidText+100)})
	}
	st := &operatorState{}
	st.seedEarlierSaid(said)
	if len(st.PersonSaid) != MaxEarlierSaid {
		t.Fatalf("%d replies kept", len(st.PersonSaid))
	}
	if n := len(st.PersonSaid[0]); n > MaxEarlierSaidText+50 {
		t.Fatalf("a reply of %d bytes kept", n)
	}
}
