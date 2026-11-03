package eval

import (
	"context"
	"strings"
	"testing"
)

// Review EVAL-1's four blockers, each with a test that fails on the code before round 2.

// B1: what a worker learned in one repeat (a method kept at a checked done) is not there in the next: the repeats are
// independent samples, so the second repeat asks again.
func TestALearnedMethodDoesNotReachTheNextRepeat(t *testing.T) {
	s, sb, _, _, _ := newService(t, diligent)
	run, err := s.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t), Repeats: 2, Cases: []string{"ask-first"},
		Arms: []Arm{{Name: "default+expert", Kind: ArmModel, Expert: true}}})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, s, run.ID)
	rows, _ := s.Store.Rows(run.ID)
	if len(rows) != 2 {
		t.Fatalf("rows: %+v", rows)
	}
	for _, r := range rows {
		if r.QRequired != 1 || !r.Passed {
			t.Fatalf("repeat %d did not ask for the cost centre again: the method of an earlier repeat reached it: %+v", r.Repeat, r)
		}
	}
	if sb.resets != 1 {
		t.Fatalf("resets: %d", sb.resets)
	}
}

// B2: a do-nothing worker that asks one question about nothing passes no case, and too many questions fail inside
// the grade.
func TestTheAskArmPassesNothing(t *testing.T) {
	s, _, hub, _, _ := newService(t, diligent)
	hub.expert = `{"fact": null}`
	run, err := s.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t), Repeats: 1, Arms: []Arm{{Name: "ask", Kind: ArmAsk}}})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, s, run.ID)
	rows, _ := s.Store.Rows(run.ID)
	if len(rows) != 3 {
		t.Fatalf("rows: %+v", rows)
	}
	for _, r := range rows {
		if r.Passed || r.Observed != OutcomeAsked {
			t.Fatalf("a question about nothing passed %s: %+v", r.CaseID, r)
		}
	}
}

func TestTooManyQuestionsFailInsideTheGrade(t *testing.T) {
	c := Case{ID: "c", Facts: []Fact{{ID: "f", About: "a", Answer: "b", Required: yes()}},
		Expected: Expected{Outcome: OutcomeComplete, Graders: []Grader{gr(t, `{"kind":"asked","facts":["f"]}`), gr(t, `{"kind":"final_text_number","number":"1"}`)}}}
	ev := Evidence{Observation: ObservedQuery, FinalText: "?", Questions: []Asked{{Kind: ObservedQuery, Fact: "f", Required: true}},
		Unfinished: []string{"too many questions"}}
	if r := Grade(c, ev, Mode{}); r.Passed {
		t.Fatalf("too many questions passed: %+v", r)
	}
	ev.Unfinished = nil
	if r := Grade(c, ev, Mode{}); !r.Passed {
		t.Fatalf("the right question did not pass: %+v", r)
	}
}

func TestLoadRequiresTheRightQuestionForEveryAcceptedOutcome(t *testing.T) {
	files := sampleSuite(t)
	edit(t, files, "cases/sum-sheet/case.json", func(m map[string]any) {
		m["expected"].(map[string]any)["accept"] = []string{"asked"}
	})
	edit(t, files, "cases/enter-bill/case.json", func(m map[string]any) {
		g := graders(m)
		g[1] = map[string]any{"kind": "asked", "mentions": []string{"ledger"}}
	})
	_, err := Load(tarFrom(t, files, nil))
	all := ""
	if p, ok := err.(Problems); ok {
		all = strings.Join(p, "\n")
	}
	for _, want := range []string{"Case sum-sheet: a case that accepts a question needs an asked grader naming the facts",
		"Case enter-bill: grader asked #2: asked names the facts"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
}

// B3: a final report answers with the number only when it states that number and no other.
func TestAFinalNumberMustBeTheAnswer(t *testing.T) {
	request := "How many support tickets are pending? Reply with the number."
	c := Case{ID: "count", Request: request, Expected: Expected{Outcome: OutcomeComplete, Graders: []Grader{gr(t, `{"kind":"final_text_number","number":"5"}`)}}}
	for text, pass := range map[string]bool{
		"5":                                   true,
		"There are 5 pending tickets.":        true,
		"Pending: 5":                          true,
		"Tickets 1 2 3 4 5 6 7 8 9 10 11 12.": false, // a list of ids
		"12 tickets; 7 open, 5 closed":        false, // a wrong count beside the right digit
		"5 of 12":                             false,
		"five":                                false, // numbers in words are not read
		"Ticket 15 is pending.":               false,
		"There are 5 pending; it already exists.": true,
	} {
		if r := Grade(c, final(text, true), Mode{}); r.Passed != pass {
			t.Errorf("%q: passed=%v, want %v (%v)", text, r.Passed, pass, r.Checks)
		}
	}
	// A number the request itself writes is not a second answer.
	c.Request = "Of the 12 tickets, how many are pending? Reply with the number."
	if r := Grade(c, final("5 of 12", true), Mode{}); !r.Passed {
		t.Fatalf("the request's own number counted against the answer: %+v", r.Checks)
	}
}

// B4: a settings change between two arms leaves the second arm's rows invalid, and the table leaves them out.
func TestASettingsChangeBetweenArmsMakesTheRunInvalid(t *testing.T) {
	s, sb, hub, _, _ := newService(t, diligent)
	sb.onCreate = func(n int) {
		if n == 2 {
			hub.mu.Lock()
			hub.deployments["azure/default"] = "big-2"
			hub.mu.Unlock()
		}
	}
	run, err := s.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t), Repeats: 1, Cases: []string{"web-save", "office"},
		Arms: []Arm{{Name: "before", Kind: ArmModel}, {Name: "after", Kind: ArmModel}}})
	if err != nil {
		t.Fatal(err)
	}
	if run.Arms[0].Deployment != "big-1" || run.Arms[1].Deployment != "big-1" {
		t.Fatalf("the run did not pin both arms at its start: %+v", run.Arms)
	}
	run = waitDone(t, s, run.ID)
	rows, _ := s.Store.Rows(run.ID)
	for _, r := range rows {
		if (r.Arm == "after") != r.Invalid {
			t.Fatalf("row %s/%s invalid=%v: %v", r.Arm, r.CaseID, r.Invalid, r.Problems)
		}
	}
	if !strings.Contains(run.Note, "invalid") {
		t.Fatalf("run note: %q", run.Note)
	}
	tab := Summarise(run, rows)
	if after := tab.Totals["after"]; after.N != 0 || after.Pass != 0 || after.Invalid != 2 {
		t.Fatalf("invalid rows counted: %+v", after)
	}
	if !strings.HasPrefix(strings.SplitN(tab.Markdown(), "\n\n", 2)[1], "**INVALID") {
		t.Fatalf("the table does not lead with INVALID:\n%s", tab.Markdown())
	}
}

// The mock's other collections must stay as the fixture left them unless a case says they may change.
func TestOtherCollectionsMustNotChange(t *testing.T) {
	c := caseWith(OutcomeComplete, gr(t, `{"kind":"mock_records","collection":"bills","records":[{"number":"B-1"}]}`))
	c.Fixture = []byte(`{"collections": {"bills": [], "users": [{"id": 1, "name": "Ann Lowe"}]}}`)
	ev := final("Saved.", true)
	ev.MockState = map[string][]map[string]any{"bills": {{"id": 1, "number": "B-1"}}, "users": {}}
	r := Grade(c, ev, Mode{})
	if r.Passed {
		t.Fatalf("deleting a user while entering a bill passed: %+v", r)
	}
	c.Expected.OthersMayChange = true
	if r := Grade(c, ev, Mode{}); !r.Passed {
		t.Fatalf("a case that lets them change still failed: %+v", r)
	}
	c.Expected.OthersMayChange = false
	ev.MockState["users"] = []map[string]any{{"id": 1, "name": "Ann Lowe"}}
	if r := Grade(c, ev, Mode{}); !r.Passed {
		t.Fatalf("an untouched collection failed: %+v", r)
	}
}

// Infrastructure failures fail the case and are counted apart, never in k/n.
func TestAnInfrastructureFailureIsCountedApart(t *testing.T) {
	c := caseWith(OutcomeComplete, gr(t, `{"kind":"final_text_number","number":"5"}`))
	r := Grade(c, Evidence{Infra: []string{"The case's mock app could not start."}}, Mode{})
	if r.Passed || !r.Infra {
		t.Fatalf("infra: %+v", r)
	}
	cellOf := cell([]Row{{Passed: true}, {Infra: true}, {Invalid: true, Passed: true}})
	if cellOf.N != 1 || cellOf.Pass != 1 || cellOf.Infra != 1 || cellOf.Invalid != 1 {
		t.Fatalf("cell: %+v", cellOf)
	}
}
