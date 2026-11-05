package skills

import (
	"strings"
	"testing"
)

// A line the task read that names a saved record beside an ordinary word ("return" in a tax return, "withdrawn" of a
// payment) raises no code point: whether a record was called off is the reviewer's to judge (review T-03).
func TestALineNamingASavedRecordRaisesNoCodePoint(t *testing.T) {
	st := &operatorState{
		Hosted:       true,
		Request:      "enter the bills",
		RecordLedger: "- INV-3309 (from bill_3309.pdf): saved, read back whole",
		Seen:         "The return for October covers invoices 3301 to 3309 as filed by the client\n",
	}
	w := (&OperatorSkill{}).weighJudgements(weighRC(&scriptedChat{replies: []string{`{"issues": []}`}}), st, nil,
		"Entered INV-3309.", "")
	for _, text := range append([]string{w.nudge, w.question}, w.open...) {
		if strings.Contains(text, "3309") {
			t.Fatalf("a code verdict on the words raised a point: %s", text)
		}
	}
}
