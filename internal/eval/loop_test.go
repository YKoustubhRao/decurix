package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
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
