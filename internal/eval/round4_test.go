package eval

import (
	"context"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// Review EVAL-3: R3-B1 and the nits N-a, N-e, N-f, each with a test that fails on the code before round 4.

// R3-B1: a throttled call (429, naming no model) between two normal answers is not another model: the row stays
// valid, and a first answer that is an error sets no pin.
func TestAThrottledCallIsNotAModelChange(t *testing.T) {
	for name, throttled := range map[string]map[int]bool{"between": {2: true}, "first": {1: true}} {
		t.Run(name, func(t *testing.T) {
			twice := func(fs *fakeSandbox, caseID, content string, n int) {
				fs.think(0)
				fs.think(0)
				diligent(fs, caseID, content, n)
			}
			s, _, hub, _, _ := newService(t, twice)
			hub.throttled = throttled
			run, err := s.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t), Repeats: 1, Cases: []string{"web-save", "office"},
				Arms: []Arm{{Name: "default", Kind: ArmModel}}})
			if err != nil {
				t.Fatal(err)
			}
			run = waitDone(t, s, run.ID)
			rows, _ := s.Store.Rows(run.ID)
			for _, r := range rows {
				if r.Invalid {
					t.Fatalf("%s: a 429 made the row invalid: %v", r.CaseID, r.Problems)
				}
			}
			if run.Arms[0].Served != "big-1-2026-01-01" {
				t.Fatalf("the pin is %q, not the first model a successful answer named", run.Arms[0].Served)
			}
		})
	}
}

// N-e: the arm's own resolution check, with an expert that does not change: a fast arm whose deployment moves
// between arms, and whose worker makes no calls, is invalid.
func TestTheArmsOwnDriftIsCaughtWhenTheExpertsIsNot(t *testing.T) {
	quiet := func(fs *fakeSandbox, caseID, content string, n int) {
		fs.save(caseID, "out.txt", "3\n")
		fs.final("Totalled.", true)
	}
	s, sb, hub, _, _ := newService(t, quiet)
	sb.onCreate = func(n int) {
		if n == 2 {
			hub.mu.Lock()
			hub.deployments["azure/fast"] = "small-2" // the expert's azure/default stays
			hub.mu.Unlock()
		}
	}
	run, err := s.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t), Repeats: 1, Cases: []string{"office"},
		Arms: []Arm{{Name: "a", Kind: ArmModel, Model: "fast"}, {Name: "b", Kind: ArmModel, Model: "fast"}}})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, s, run.ID)
	rows, _ := s.Store.Rows(run.ID)
	if rows[0].Invalid || !rows[1].Invalid {
		t.Fatalf("rows: %v / %v", rows[0].Problems, rows[1].Problems)
	}
}

// N-e: the arm's own endpoint check, on a provider the expert does not use.
func TestTheArmsOwnEndpointIsCaughtWhenTheExpertsIsNot(t *testing.T) {
	quiet := func(fs *fakeSandbox, caseID, content string, n int) {
		fs.save(caseID, "out.txt", "3\n")
		fs.final("Totalled.", true)
	}
	s, sb, hub, _, _ := newService(t, quiet)
	hub.deployments["claude/default"] = "claude-big"
	sb.onCreate = func(n int) {
		if n == 2 {
			hub.mu.Lock()
			hub.endpoints = map[string]string{"claude": "https://elsewhere.example.test"}
			hub.mu.Unlock()
		}
	}
	run, err := s.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t), Repeats: 1, Cases: []string{"office"},
		Arms: []Arm{{Name: "a", Kind: ArmModel, Provider: "claude"}, {Name: "b", Kind: ArmModel, Provider: "claude"}}})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, s, run.ID)
	rows, _ := s.Store.Rows(run.ID)
	if rows[0].Invalid || !rows[1].Invalid {
		t.Fatalf("rows: %v / %v", rows[0].Problems, rows[1].Problems)
	}
}

// N-a and N-f: evidence the runner could not read is infrastructure, out of k/n and never a false done, and the
// task's recorded grade says its check was not made.
func TestEvidenceThatCouldNotBeReadIsInfrastructure(t *testing.T) {
	c := caseWith(OutcomeComplete, gr(t, `{"kind":"final_text_number","number":"5"}`))
	yes := true
	r := Grade(c, Evidence{Observation: ObservedFinal, FinalText: "5", Checked: &yes, CollectProblems: []string{"The case's files could not be collected."}}, Mode{})
	if r.Passed || !r.Infra || r.FalseDone || r.DoneUncheckedWrong {
		t.Fatalf("a collection failure: %+v", r)
	}

	s, sb, _, _, _ := newService(t, diligent)
	tk := &evalTasks{}
	s.Tasks = tk
	sb.failCollect = true
	run, err := s.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t), Repeats: 1, Cases: []string{"office"},
		Arms: []Arm{{Name: "default", Kind: ArmModel}}})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, s, run.ID)
	rows, _ := s.Store.Rows(run.ID)
	if len(rows) != 1 || !rows[0].Infra || rows[0].FalseDone || rows[0].Passed {
		t.Fatalf("row: %+v", rows)
	}
	tk.mu.Lock()
	defer tk.mu.Unlock()
	v := tk.outcomes[rows[0].TaskID].Verdict
	if v == nil || v.Verdict != tasks.GateUnchecked || v.Checks[0].Result != "not_made" {
		t.Fatalf("the recorded grade: %+v", v)
	}
	if cell := Summarise(run, rows).Totals["default"]; cell.N != 0 || cell.Infra != 1 {
		t.Fatalf("cell: %+v", cell)
	}
}
