package eval

import (
	"context"
	"strings"
	"testing"
)

// The expert's reply reaches the worker as a person's does: with the judgement id the broker serves it back under
// (practice.Broker.KeepReply), so what it says backs a value on that task and the worker's later ones; the worker's
// own requests carry none.
func TestTheExpertsReplyCarriesItsJudgement(t *testing.T) {
	s, sb, _, _, _ := newService(t, diligent)
	run, err := s.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t), Repeats: 2, Cases: []string{"ask-first"},
		Arms: []Arm{{Name: "default+expert", Kind: ArmModel, Expert: true}}})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, s, run.ID)
	sb.mu.Lock()
	defer sb.mu.Unlock()
	ids := map[string]bool{}
	for _, req := range sb.requests {
		id, replied := req["judgement_id"]
		if !replied {
			continue
		}
		if !strings.HasPrefix(id, "j_") || ids[id] || req["content"] == "" || req["task_id"] == "" {
			t.Fatalf("a reply's judgement: %+v", req)
		}
		ids[id] = true
	}
	if len(ids) != 2 || len(sb.requests) <= len(ids) {
		t.Fatalf("%d replies carried a judgement among %d requests: %+v", len(ids), len(sb.requests), sb.requests)
	}
}
