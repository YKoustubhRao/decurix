package eval

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// Review EVAL-2's points (N3-N6), each with a test that fails on the code before round 3.

// N4: the model the upstream says served an answer is pinned with the deployment's name: an upgrade behind the same
// name marks the rows invalid.
func TestAnotherServedModelUnderTheSameDeploymentIsInvalid(t *testing.T) {
	s, _, hub, _, _ := newService(t, diligent)
	hub.served = "gpt-big-2026-01"
	calls := 0
	sbWork := diligent
	s.Sandbox.(*fakeSandbox).work = func(fs *fakeSandbox, caseID, content string, n int) {
		calls++
		if calls == 2 {
			hub.mu.Lock()
			hub.served = "gpt-big-2026-06"
			hub.mu.Unlock()
		}
		sbWork(fs, caseID, content, n)
	}
	run, err := s.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t), Repeats: 1, Cases: []string{"web-save", "office"},
		Arms: []Arm{{Name: "default", Kind: ArmModel}}})
	if err != nil {
		t.Fatal(err)
	}
	run = waitDone(t, s, run.ID)
	rows, _ := s.Store.Rows(run.ID)
	if rows[0].Invalid || !rows[1].Invalid || !strings.Contains(run.Note, "invalid") {
		t.Fatalf("rows: %v %v; note %q", rows[0].Problems, rows[1].Problems, run.Note)
	}
	if run.Arms[0].Served != "gpt-big-2026-01" {
		t.Fatalf("the arm's served model: %+v", run.Arms[0])
	}
}

// N4: an endpoint change between arms marks the later arm invalid; a key change alone (same endpoint, same
// deployment, same served model) does not.
func TestAnEndpointChangeBetweenArmsIsInvalid(t *testing.T) {
	for _, tc := range []struct {
		name     string
		endpoint string
		invalid  bool
	}{{"endpoint", "https://other-region.example.test", true}, {"key only", "", false}} {
		t.Run(tc.name, func(t *testing.T) {
			s, sb, hub, _, _ := newService(t, diligent)
			sb.onCreate = func(n int) {
				if n == 2 && tc.endpoint != "" {
					hub.mu.Lock()
					hub.endpoint = tc.endpoint
					hub.mu.Unlock()
				}
			}
			run, err := s.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t), Repeats: 1, Cases: []string{"office"},
				Arms: []Arm{{Name: "a", Kind: ArmModel}, {Name: "b", Kind: ArmModel}}})
			if err != nil {
				t.Fatal(err)
			}
			waitDone(t, s, run.ID)
			rows, _ := s.Store.Rows(run.ID)
			if rows[0].Invalid || rows[1].Invalid != tc.invalid {
				t.Fatalf("rows: %+v %+v", rows[0].Problems, rows[1].Problems)
			}
		})
	}
}

// N3: an arm that makes no model call at all is still held to the run's pins at its start.
func TestTheArmStartCheckCatchesADriftWithNoCalls(t *testing.T) {
	quiet := func(fs *fakeSandbox, caseID, content string, n int) {
		fs.save(caseID, "out.txt", "3\n")
		fs.final("Totalled.", true)
	}
	s, sb, hub, _, _ := newService(t, quiet)
	sb.onCreate = func(n int) {
		if n == 2 {
			hub.mu.Lock()
			hub.deployments["azure/default"] = "big-2"
			hub.mu.Unlock()
		}
	}
	run, err := s.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t), Repeats: 1, Cases: []string{"office"},
		Arms: []Arm{{Name: "a", Kind: ArmModel}, {Name: "b", Kind: ArmModel}}})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, s, run.ID)
	rows, _ := s.Store.Rows(run.ID)
	if len(rows[1].Deployments) != 0 || rows[0].Invalid || !rows[1].Invalid {
		t.Fatalf("rows: %+v / %+v", rows[0], rows[1])
	}
}

// N5: a date or a time of day in the answer is not a second number.
func TestDatesAndTimesAreNotOtherNumbers(t *testing.T) {
	c := Case{ID: "count", Request: "How many support tickets are pending? Reply with the number.",
		Expected: Expected{Outcome: OutcomeComplete, Graders: []Grader{gr(t, `{"kind":"final_text_number","number":"5"}`)}}}
	for text, pass := range map[string]bool{
		"5 tickets are pending as of 2026-11-04.":  true,
		"5 pending (checked 04/11/2026 at 10:30).": true,
		"Pending: 5, as at 4.11.26 09:15:02":       true,
		"Pending as of 2026-11-05.":                false, // the 5 is the date's, not an answer
		"5 pending, 7 open as of 2026-11-04.":      false,
	} {
		if r := Grade(c, final(text, true), Mode{}); r.Passed != pass {
			t.Errorf("%q: passed=%v, want %v (%v)", text, r.Passed, pass, r.Checks)
		}
	}
}

// N6: infrastructure rows stay out of k/n, and any of them makes the result INCOMPLETE, the count beside k/n.
func TestInfrastructureRowsMakeTheTableIncomplete(t *testing.T) {
	run, rows := fixtureRun(5)
	rows[0].Infra, rows[0].Passed = true, false
	tab := Summarise(run, rows)
	md := tab.Markdown()
	if tab.Infra != 1 || !strings.Contains(md, "**INCOMPLETE: 1 case run(s)") {
		t.Fatalf("not marked incomplete:\n%s", md)
	}
	if one := tab.Rows[0].Arms["default"]; one.N != 4 || one.Infra != 1 {
		t.Fatalf("cell: %+v", one)
	}
	if !strings.Contains(md, "| one | 4/4 (lo 0.51) +1 infra |") {
		t.Fatalf("the infra count is not beside k/n:\n%s", md)
	}
	if Summarise(run, rows[1:]).Infra != 0 || strings.Contains(Summarise(run, rows[1:]).Markdown(), "INCOMPLETE") {
		t.Fatal("a complete run says INCOMPLETE")
	}
}

// N6: the worker's agent refusing a delivered request is the worker's failure, not infrastructure.
func TestAnAgentRefusalIsTheWorkersFailure(t *testing.T) {
	ev := Evidence{}
	notDelivered(&ev, agentRefused(500), "The request could not be sent to the worker.")
	if len(ev.Unfinished) != 1 || len(ev.Infra) != 0 {
		t.Fatalf("a refusal: %+v", ev)
	}
	ev = Evidence{}
	notDelivered(&ev, context.DeadlineExceeded, "The request could not be sent to the worker.")
	if len(ev.Infra) != 1 || len(ev.Unfinished) != 0 {
		t.Fatalf("a connection failure: %+v", ev)
	}
}

// evalTasks records the outcomes the runner writes on its cases' task records.
type evalTasks struct {
	mu       sync.Mutex
	outcomes map[string]tasks.Outcome
}

func (e *evalTasks) CommitTyped(route tasks.Route, _ tasks.Worker, _, _ string, _ tasks.Origin) (tasks.Route, error) {
	return route, nil
}
func (e *evalTasks) AddStep(string, string, tasks.Step) (bool, error) { return true, nil }
func (e *evalTasks) RecordOutcome(_, taskID string, o tasks.Outcome) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.outcomes == nil {
		e.outcomes = map[string]tasks.Outcome{}
	}
	e.outcomes[taskID] = o
	return taskID, nil
}
func (e *evalTasks) CloseOpen(string, string) error { return nil }

// H-CP-2: an eval row's grade reaches its task record as a verdict by eval, never a bare checked.
func TestAnEvalGradeIsRecordedAsAVerdict(t *testing.T) {
	s, _, _, _, _ := newService(t, diligent)
	tk := &evalTasks{}
	s.Tasks = tk
	run, err := s.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t), Repeats: 1, Arms: []Arm{{Name: "nothing", Kind: ArmNothing}, {Name: "default", Kind: ArmModel}}})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, s, run.ID)
	rows, _ := s.Store.Rows(run.ID)
	tk.mu.Lock()
	defer tk.mu.Unlock()
	for _, r := range rows {
		o, ok := tk.outcomes[r.TaskID]
		if !ok || o.Checked != nil || o.Verdict == nil || o.Verdict.By != tasks.ByEval {
			t.Fatalf("%s/%s: outcome %+v", r.Arm, r.CaseID, o)
		}
		if (o.Verdict.Verdict == tasks.GateDone) != r.Passed {
			t.Fatalf("%s/%s: passed=%v, verdict %s", r.Arm, r.CaseID, r.Passed, o.Verdict.Verdict)
		}
	}
}
