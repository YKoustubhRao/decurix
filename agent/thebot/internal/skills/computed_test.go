package skills

import (
	"strings"
	"testing"
)

// A typed value that only code the work wrote itself printed is named to the reviewer, as a fact (review T-31); a value
// a source held is not.
func TestAValueOnlyTheWorksOwnCodePrintedIsAFactForTheReviewer(t *testing.T) {
	st := &operatorState{Request: "enter the total of 12340 and 5; the reference is 7781"}
	st.noteSeen(st.Request)
	command := `python3 -c "print(12340+5, 7781)"`
	shown := withoutCommandValues("12345 7781", command)
	st.noteComputed(command, shown)
	st.noteSeen(shown)
	st.noteTracedTyping(`gui type e5 "12345" && gui type e6 "7781"`)
	if len(st.ComputedTyped) != 1 || st.ComputedTyped[0] != "12345" {
		t.Fatalf("computed typed = %v", st.ComputedTyped)
	}
	if brief := st.reviewFacts("Done.", nil, ""); !strings.Contains(brief, "ONLY CODE THE WORK WROTE ITSELF PRINTED") ||
		!strings.Contains(brief, "12345") {
		t.Errorf("the reviewer is not told:\n%s", brief)
	}
	plain := &operatorState{}
	plain.noteComputed("cat totals.txt", "99999")
	if len(plain.ComputedOnly) != 0 {
		t.Error("a file's output was taken for the work's own code")
	}
}
