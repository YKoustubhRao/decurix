package skills

import (
	"strings"
	"testing"
)

// The question a refused typing ends in names what was about to be typed, on every path. Found live (2026-10-20,
// round 5 P2): a planned stage asked "Which document or page should I take it from?" naming nothing; the person could
// not answer it, and a reply about something else was taken for its answer.
func TestTheQuestionAfterARefusedTypingNamesTheValue(t *testing.T) {
	// A document the request names, never read.
	st := groundingState("Enter the order from /home/headless/work/P2/HH-PO-7781.pdf in the portal.")
	nudge, values := groundingCheck(st, `gui type e9 "HH-PO-7781-B"`)
	if nudge == "" || len(values) == 0 {
		t.Fatalf("an unread document's refusal handed over no values: %q %v", nudge, values)
	}
	q := untracedQuestion(values, nudge)
	if !strings.Contains(q, `"HH-PO-7781-B"`) || !strings.Contains(q, "could not read HH-PO-7781.pdf") || strings.Contains(q, "Not run") {
		t.Errorf("question = %q", q)
	}

	// A value the task never showed.
	st = groundingState("Enter the purchase order from the supplier.")
	nudge, values = groundingCheck(st, `gui type e9 "Green Furnishings"`)
	if q := untracedQuestion(values, nudge); !strings.Contains(q, `"Green Furnishings"`) {
		t.Errorf("question = %q", q)
	}

	// The old bare question is gone: even with no values it says what stopped it.
	q = untracedQuestion(nil, "Not run: something about the value")
	if strings.Contains(q, "some of what I was about to type") || !strings.Contains(q, "something about the value") {
		t.Errorf("a question naming nothing: %q", q)
	}
}
