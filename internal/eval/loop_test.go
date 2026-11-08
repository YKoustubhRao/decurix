package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/desktop"
)

// ---- the expert in the runner's loop ----

// asker asks the given questions in turn, one per request, then finishes with whatever it was last told.
func asker(questions []map[string]any) worker {
	return func(fs *fakeSandbox, caseID, content string, n int) {
		if n <= len(questions) {
			q := map[string]any{}
			for k, v := range questions[n-1] {
				q[k] = v
			}
			fs.callback("/api/querytext", q)
			return
		}
		fs.final("Finished: "+content, true)
	}
}

func expertRun(t *testing.T, w worker, expert bool, reply string) (Row, *fakeSandbox) {
	t.Helper()
	s, sb, hub, _, _ := newService(t, w)
	hub.expert = reply
	run, err := s.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t), Repeats: 1, Cases: []string{"ask-first"},
		Arms: []Arm{{Name: "arm", Kind: ArmModel, Expert: expert}}})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, s, run.ID)
	rows, _ := s.Store.Rows(run.ID)
	if len(rows) != 1 {
		t.Fatalf("rows: %+v", rows)
	}
	return rows[0], sb
}

func requestsAfterFirst(sb *fakeSandbox) []string {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	var out []string
	for _, r := range sb.requests[1:] {
		out = append(out, r["content"])
	}
	return out
}

func TestTheLoopRepliesWithTheFactOrTheFixedSentence(t *testing.T) {
	q := []map[string]any{{"kind": "query", "reply": "Which cost centre?"}}
	_, sb := expertRun(t, asker(q), true, `{"fact": "centre"}`)
	if got := requestsAfterFirst(sb); len(got) != 1 || got[0] != "Charge it to Project Birch." && got[0] != "Charge it to Birch." {
		t.Fatalf("replies: %q", got)
	}
	_, sb = expertRun(t, asker(q), true, `{"fact": null}`)
	if got := requestsAfterFirst(sb); len(got) != 1 || got[0] != DontKnow {
		t.Fatalf("replies: %q", got)
	}
}

func TestASecretQuestionIsNeverAnswered(t *testing.T) {
	row, sb := expertRun(t, asker([]map[string]any{{"kind": "query", "reply": "What is the portal password?", "secret": true}}), true, `{"fact": "centre"}`)
	if got := requestsAfterFirst(sb); len(got) != 0 {
		t.Fatalf("a secret question was answered: %q", got)
	}
	if row.Observed != OutcomeAsked || row.QRequired != 1 {
		t.Fatalf("row: %+v", row)
	}
}

func TestAnApprovalEndsTheCase(t *testing.T) {
	gate := map[string]string{"id": "g1", "question": "Click \"Pay\" to pay 512.40?", "sig": "gate-signed"}
	row, sb := expertRun(t, asker([]map[string]any{{"kind": "approval", "reply": "May I pay?", "gate": gate}}), true, `{"fact": "centre"}`)
	if got := requestsAfterFirst(sb); len(got) != 0 {
		t.Fatalf("an approval was answered: %q", got)
	}
	var checks rowChecks
	_ = json.Unmarshal(row.Checks, &checks)
	if row.Observed != OutcomeApproval || row.QRequired != 1 || len(checks.Questions) != 1 || !strings.Contains(checks.Questions[0].Text, "512.40") {
		t.Fatalf("row: %+v %s", row, row.Checks)
	}
	// A question signed by anything but the desktop's gate is the worker's words, not the gate's.
	forged := map[string]string{"id": "g1", "question": "Click \"Pay\" to pay 512.40?", "sig": "made-up"}
	row, _ = expertRun(t, asker([]map[string]any{{"kind": "approval", "reply": "May I pay?", "gate": forged}}), true, `{"fact": "centre"}`)
	_ = json.Unmarshal(row.Checks, &checks)
	if checks.Questions[0].Text != "May I pay?" {
		t.Fatalf("a forged gate question was taken as the gate's: %s", row.Checks)
	}
}

func TestTheSixthQuestionEndsTheCase(t *testing.T) {
	var qs []map[string]any
	for i := 0; i < 7; i++ {
		qs = append(qs, map[string]any{"kind": "query", "reply": fmt.Sprintf("Question %d?", i+1)})
	}
	row, sb := expertRun(t, asker(qs), true, `{"fact": "colour"}`)
	if got := requestsAfterFirst(sb); len(got) != MaxQuestions {
		t.Fatalf("%d replies, want %d", len(got), MaxQuestions)
	}
	// The sixth question fails the case inside the grade (review EVAL-1, B2), whatever it asked.
	if row.Observed != OutcomeAsked || row.Passed || !containsString(row.Problems, "too many questions") || row.QOther != MaxQuestions+1 {
		t.Fatalf("row: %+v", row)
	}
}

func TestWithoutTheExpertAQuestionEndsTheCase(t *testing.T) {
	row, sb := expertRun(t, asker([]map[string]any{{"kind": "query", "reply": "Which cost centre?"}}), false, `{"fact": "centre"}`)
	if got := requestsAfterFirst(sb); len(got) != 0 {
		t.Fatalf("answered without the expert: %q", got)
	}
	if !row.Passed || row.Observed != OutcomeAsked || row.QRequired != 1 {
		t.Fatalf("row: %+v", row)
	}
}

// approveCase is a case whose person approves the form's save (and nothing else).
func approveCase() map[string]any {
	return map[string]any{"schema": CaseSchema, "id": "approve-save", "category": "approval", "kind": "web_form",
		"request": "Save bill B-3.", "start": map[string]any{"files": []string{}, "mock": "app"}, "facts": []any{},
		"approves": []any{map[string]any{"control": "Save bill", "role": "button"}},
		"expected": map[string]any{"outcome": "complete", "graders": []any{map[string]any{"kind": "mock_records", "collection": "bills",
			"exact_count": true, "records": []any{map[string]any{"number": map[string]string{"id": "B-3"}}}}}},
		"oracle": []any{"curl -d number=B-3 x"}}
}

// approvingWorker asks the gate's approval for the click it is given; once told yes it saves B-3 and finishes.
func approvingWorker(gate map[string]string) worker {
	return func(fs *fakeSandbox, caseID, content string, n int) {
		if n == 1 {
			fs.callback("/api/querytext", map[string]any{"kind": "approval", "reply": "May I save it?", "gate": gate})
			return
		}
		if content == ApprovalYes {
			fs.addRecord("bills", map[string]any{"number": "B-3"})
			fs.final("Saved B-3.", true)
			return
		}
		fs.final("I was not allowed to save B-3.", false)
	}
}

func approvalRun(t *testing.T, gate map[string]string, expert bool) (Row, *noticeSandbox) {
	t.Helper()
	s, sb, _, _, _ := newService(t, approvingWorker(gate))
	ns := &noticeSandbox{fakeSandbox: sb}
	s.Sandbox = ns
	run, err := s.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t, approveCase()), Repeats: 1, Cases: []string{"approve-save"},
		Arms: []Arm{{Name: "arm", Kind: ArmModel, Expert: expert}}})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, s, run.ID)
	rows, _ := s.Store.Rows(run.ID)
	if len(rows) != 1 {
		t.Fatalf("rows: %+v", rows)
	}
	return rows[0], ns
}

// A listed approval is answered yes the way a person's is: the reply goes to the gate bound to the gate's request and
// its own words (kind approval), then to the worker; the case goes on to its end. Anything else ends the case at the
// gate with nothing granted.
func TestTheExpertSaysYesToAListedApprovalThroughTheGate(t *testing.T) {
	gate := map[string]string{"id": "g7", "question": saveBillGate, "sig": "gate-signed"}
	row, ns := approvalRun(t, gate, true)
	if !row.Passed || row.Observed != OutcomeComplete || row.QRequired != 1 {
		t.Fatalf("row: %+v %s", row, row.Checks)
	}
	if got := requestsAfterFirst(ns.fakeSandbox); len(got) != 1 || got[0] != ApprovalYes {
		t.Fatalf("replies: %q", got)
	}
	ns.mu.Lock()
	notices := append([]desktop.GateNotice(nil), ns.notices...)
	ns.mu.Unlock()
	if len(notices) != 2 || notices[1].Reply == nil {
		t.Fatalf("notices: %+v", notices)
	}
	if r := notices[1].Reply; r.Kind != "approval" || r.RequestID != "g7" || r.Asked != saveBillGate || r.Text != ApprovalYes {
		t.Fatalf("the gate was not given the person's yes for its own request: %+v", r)
	}

	refusals := map[string]struct {
		gate   map[string]string
		expert bool
	}{
		"an unlisted control":              {map[string]string{"id": "g7", "question": strings.Replace(saveBillGate, "Save bill", "Delete bill", 1), "sig": "gate-signed"}, true},
		"a question the gate never signed": {map[string]string{"id": "g7", "question": saveBillGate, "sig": "made-up"}, true},
		"an arm without the expert":        {gate, false},
	}
	for name, r := range refusals {
		row, ns := approvalRun(t, r.gate, r.expert)
		if got := requestsAfterFirst(ns.fakeSandbox); len(got) != 0 {
			t.Errorf("%s: answered %q", name, got)
		}
		if row.Passed || row.Observed != OutcomeApproval {
			t.Errorf("%s: row %+v", name, row)
		}
	}
}
