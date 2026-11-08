package skills

// Tests for the fields of a taught task filled by picking an application's entry (RunContext.PickedFields): the
// review is told them as a fact, bounded.

import (
	"strings"
	"testing"
)

// The review is told which fields hold an entry picked from the application's list, with the words that searched
// for it: a different name there is the entry's own, no point.
func TestTheReviewIsToldWhichFieldsArePicked(t *testing.T) {
	st := &operatorState{Request: "Enter the bill"}
	st.seedPicked([]string{`Item (row 2): searched with "Mid Back Ergonomic Office Chair with Headrest"`, "  "})
	if len(st.Picked) != 1 {
		t.Fatalf("Picked = %q", st.Picked)
	}
	facts := st.reviewFacts("DONE: entered", nil, "")
	if !strings.Contains(facts, "FIELDS FILLED BY PICKING AN EXISTING ENTRY") ||
		!strings.Contains(facts, `Item (row 2): searched with "Mid Back`) {
		t.Fatalf("the review is not told the picked field:\n%s", facts)
	}
	if strings.Contains((&operatorState{}).reviewFacts("DONE", nil, ""), "FIELDS FILLED BY PICKING") {
		t.Fatal("a job with no picked field is told of some")
	}
}

// What a job is sent is bounded: at most MaxPickedFields, each cut to MaxPickedFieldText.
func TestPickedFieldsAreBounded(t *testing.T) {
	var picked []string
	for i := 0; i < MaxPickedFields+5; i++ {
		picked = append(picked, strings.Repeat("x", MaxPickedFieldText+100))
	}
	st := &operatorState{}
	st.seedPicked(picked)
	if len(st.Picked) != MaxPickedFields {
		t.Fatalf("%d kept", len(st.Picked))
	}
	if n := len(st.Picked[0]); n > MaxPickedFieldText+50 {
		t.Fatalf("a field of %d bytes kept", n)
	}
}
