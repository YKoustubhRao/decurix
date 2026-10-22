package skills

import (
	"strings"
	"testing"

	"thebot/internal/llm"
)

// The reviewer is shown facts gathered by code and decides itself; code only reads its answer.

func TestTheReviewerIsShownTheFactsTheGoalHolds(t *testing.T) {
	st := &operatorState{Request: "file the month's return", PrefixLen: 1,
		Messages: []llm.Message{{Role: "system", Content: "Lesson: rent is never a purchase credit."},
			{Role: "user", Content: "contract"}},
		Ledger:     []string{"$ office read sales.xlsx -> 455 CANCELLED"},
		Commands:   []string{`curl -fsSLo 'Invoice_451.pdf' http://x/451`},
		ScreenDocs: []string{"Invoice_457.pdf"},
		Lookups:    []webLookup{{Command: "web search gstr-1 cancelled", Links: []string{"https://example.org/help"}}},
		Seen:       "455 CANCELLED"}
	facts := st.reviewFacts("Filed.", []string{`Rent kept out | backed by: "a rule nobody gave"`}, "click FILE")
	for _, want := range []string{"file the month's return", "rent is never a purchase credit", "455 CANCELLED",
		"Invoice_451.pdf", "Invoice_457.pdf", "https://example.org/help", "APPEAR IN NO TEXT", "click FILE", "not done yet, by design", "Filed."} {
		if !strings.Contains(facts, want) {
			t.Errorf("facts lack %q:\n%s", want, facts)
		}
	}
}

func TestTheReviewerSeesWhereACitedQuoteStandsInTheFullOutput(t *testing.T) {
	// GST suite T3: the figures were deep in a screen read's output; the ledger kept only its first words.
	shown := "gui observe\n" + strings.Repeat("e12 link \"Menu\"\n", 200) +
		"e3997 row \"Electronic Credit Ledger | 0.00 | 42,933.50 | 42,933.50 | 0.00\"\ne3998 row \"Cash Ledger | 0.00\"\n"
	got := quotedPassages([]string{
		`Ledger agrees | backed by: "Electronic Credit Ledger | 0.00 | 42,933.50" (portal)`,
		`Kept the rate | backed by: "a rule nobody gave"`,
	}, shown)
	if len(got) != 1 || !strings.Contains(got[0], `e3997 row "Electronic Credit Ledger | 0.00 | 42,933.50 | 42,933.50 | 0.00"`) ||
		!strings.Contains(got[0], "Cash Ledger") {
		t.Fatalf("passages = %q", got)
	}
	// A short quote found by its words standing close together still points at its place.
	if got := quotedPassages([]string{`Karnataka | backed by: "O62='Karnataka'"`},
		`[SEPTEMBER]  K62==IF(O62="Karnataka",ROUND(I62*H62/200,2),0) -> 15254.24`); len(got) != 1 || !strings.Contains(got[0], "15254.24") {
		t.Errorf("short quote passages = %q", got)
	}
}

func TestTheReviewersAnswerIsReadAndAFailedReviewDecidesNothing(t *testing.T) {
	st := &operatorState{Request: "x", Messages: []llm.Message{{Role: "user", Content: "x"}}}
	type want struct {
		issues   int
		reviewed bool
	}
	cases := map[string]want{
		"Here: {\"issues\": [{\"what\": \"Invoice_457 never read\", \"settle\": \"recheck\"}, {\"what\": \" \"}]}": {1, true},
		`{"issues": []}`:      {0, true},
		"I could not decide.": {0, false}, // tried twice ("out of script" the second time), then said to be unmade
	}
	for reply, w := range cases {
		rc := operatorContext(t, &scriptedChat{replies: []string{reply}})
		if got, ok := NewOperator().review(rc, st, "done", nil, ""); len(got) != w.issues || ok != w.reviewed {
			t.Errorf("%q: %d issues reviewed %v, want %+v", reply, len(got), ok, w)
		}
	}
	// An unreadable first answer is asked again.
	rc := operatorContext(t, &scriptedChat{replies: []string{"thinking...", `{"issues": [{"what": "x"}]}`}})
	if got, ok := NewOperator().review(rc, st, "done", nil, ""); len(got) != 1 || !ok {
		t.Errorf("a second try was not made: %v %v", got, ok)
	}
}
