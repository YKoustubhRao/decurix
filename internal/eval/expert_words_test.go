package eval

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/tasks"
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

// judgingTasks is a task store that also keeps judgements, as the control plane's does.
type judgingTasks struct {
	evalTasks
	jmu        sync.Mutex
	judgements []tasks.Judgement
}

func (j *judgingTasks) AddJudgement(x tasks.Judgement) (string, error) {
	j.jmu.Lock()
	defer j.jmu.Unlock()
	j.judgements = append(j.judgements, x)
	return x.ID, nil
}

// The expert's reply is in the task records as an answer authored by the scripted expert, under the id the worker was
// sent, naming the fact it gave (plan rule 5): the run record shows who said what.
func TestTheExpertsReplyIsRecordedWithItsAuthor(t *testing.T) {
	s, sb, _, _, _ := newService(t, diligent)
	tk := &judgingTasks{}
	s.Tasks = tk
	run, err := s.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t), Repeats: 1, Cases: []string{"ask-first"},
		Arms: []Arm{{Name: "default+expert", Kind: ArmModel, Expert: true}}})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, s, run.ID)
	sb.mu.Lock()
	sent := map[string]string{}
	for _, req := range sb.requests {
		if id, ok := req["judgement_id"]; ok {
			sent[id] = req["content"]
		}
	}
	sb.mu.Unlock()
	tk.jmu.Lock()
	defer tk.jmu.Unlock()
	if len(tk.judgements) != 1 || len(sent) != 1 {
		t.Fatalf("judgements %+v for replies %+v", tk.judgements, sent)
	}
	j := tk.judgements[0]
	if j.Actor.Kind != tasks.ActorScripted || j.Actor.ID != ExpertActor.ID || j.Kind != tasks.JudgementAnswer || j.Door != "eval" {
		t.Fatalf("the judgement's author or kind: %+v", j)
	}
	if text, ok := sent[j.ID]; !ok || text != j.Text || j.TaskID == "" || j.WorkerID == "" {
		t.Fatalf("the judgement %+v is not the reply sent (%+v)", j, sent)
	}
	var payload map[string]string
	if err := json.Unmarshal(j.Payload, &payload); err != nil || payload["asked"] == "" {
		t.Fatalf("the judgement's payload: %s (%v)", j.Payload, err)
	}
}
