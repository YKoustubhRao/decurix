package eval

import "testing"

// A question names a fact only by the case's own refers_to: whole words, any case, every word of one phrase (F-L2-2).
func TestAQuestionNamesAFactByItsRefersTo(t *testing.T) {
	f := Fact{ID: "centre", RefersTo: []string{"centre", "center", "cost code"}}
	for q, want := range map[string]bool{
		"Which cost CENTRE should this bill go to?": true,
		"Which cost-center is it?":                  true,
		"What is the code for its cost?":            true,
		"Is the code right?":                        false, // one word of a two-word phrase
		"Should I use the city centres list?":       false, // whole words only
		AskAnything:                                 false,
		"":                                          false,
	} {
		if got := f.NamedBy(q); got != want {
			t.Errorf("%q: %v, want %v", q, got, want)
		}
	}
}

// The asked check says what decided it: the case's data when the question names none of its facts, the expert's
// choice when it names one.
func TestTheAskedCheckSaysWhatDecidedIt(t *testing.T) {
	c := caseWith(OutcomeComplete, gr(t, `{"kind":"final_text_number","number":"1"}`), gr(t, `{"kind":"asked","facts":["ledger"],"when":"asked"}`))
	c.Expected.Accept = []string{OutcomeAsked}
	ask := func(q, fact string) Check {
		r := Grade(c, Evidence{Observation: ObservedQuery, FinalText: q, Questions: []Asked{{Kind: ObservedQuery, Text: q, Fact: fact}}}, Mode{})
		return r.Checks[1]
	}
	if ch := ask("Which ledger?", "ledger"); ch.Result != CheckPass || ch.Trust != TrustModel {
		t.Fatalf("named and chosen: %+v", ch)
	}
	if ch := ask("Which ledger?", ""); ch.Result != CheckFail || ch.Trust != TrustModel {
		t.Fatalf("named, not chosen: %+v", ch)
	}
	// An expert's match to a question that names nothing the fact refers to never counts, and no model decided it.
	if ch := ask("Anything else?", "ledger"); ch.Result != CheckFail || ch.Trust != TrustOutput {
		t.Fatalf("not named: %+v", ch)
	}
}
